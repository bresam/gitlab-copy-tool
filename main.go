// Command gitlab-copy-tool migrates a self-hosted GitLab instance to GitLab
// SaaS: it copies repositories (all branches and tags) version-independently
// via git mirror, recreates the group/folder structure, and optionally copies
// issues, CI variables and settings.
package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/bresam/gitlab-copy-tool/internal/config"
	"github.com/bresam/gitlab-copy-tool/internal/gitlabapi"
	"github.com/bresam/gitlab-copy-tool/internal/migrate"
	"github.com/bresam/gitlab-copy-tool/internal/tui"
	"github.com/spf13/cobra"
)

func main() {
	if err := rootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func rootCmd() *cobra.Command {
	var dryRun bool
	root := &cobra.Command{
		Use:   "gitlab-copy-tool",
		Short: "Migrate a self-hosted GitLab to GitLab SaaS",
		Long: "Interactive tool to migrate repositories (all branches & tags), group\n" +
			"structure, issues, CI variables and settings from a self-hosted GitLab\n" +
			"to GitLab SaaS. Run without arguments to start the interactive UI.\n" +
			"With --dry-run the interactive UI resolves the plan without pushing.",
		RunE: func(cmd *cobra.Command, args []string) error {
			return tui.Run(dryRun)
		},
	}
	root.Flags().BoolVar(&dryRun, "dry-run", false, "start the interactive UI in dry-run mode (no push)")
	root.AddCommand(sessionsCmd(), runCmd())
	return root
}

func sessionsCmd() *cobra.Command {
	c := &cobra.Command{Use: "sessions", Short: "Manage saved sessions"}

	c.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List saved sessions",
		RunE: func(cmd *cobra.Command, args []string) error {
			names, err := config.List()
			if err != nil {
				return err
			}
			if len(names) == 0 {
				fmt.Println("(no saved sessions)")
				return nil
			}
			for _, n := range names {
				fmt.Println(n)
			}
			return nil
		},
	})

	c.AddCommand(&cobra.Command{
		Use:   "rm <name>",
		Short: "Delete a saved session",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return config.Remove(args[0])
		},
	})

	return c
}

func runCmd() *cobra.Command {
	var sessionName string
	var dryRun bool
	var pathMapFile string

	c := &cobra.Command{
		Use:   "run",
		Short: "Run a saved session non-interactively",
		RunE: func(cmd *cobra.Command, args []string) error {
			if sessionName == "" {
				// No session: fall back to the interactive UI so the user can
				// pick or create one. Only meaningful with --dry-run (a real
				// run without a chosen session would have nothing to do).
				if dryRun {
					return tui.Run(true)
				}
				return fmt.Errorf("--session is required (or use --dry-run to pick one interactively)")
			}
			return runSession(sessionName, dryRun, pathMapFile)
		},
	}
	c.Flags().StringVar(&sessionName, "session", "", "name of the saved session to run")
	c.Flags().BoolVar(&dryRun, "dry-run", false, "resolve the plan without pushing (without --session: open the interactive UI in dry-run mode)")
	c.Flags().StringVar(&pathMapFile, "path-map", "", "extra old->new path map JSON merged on top of the session's own map for the URL rewrite")
	return c
}

func runSession(name string, dryRun bool, pathMapFile string) error {
	sess, err := config.Load(name)
	if err != nil {
		return fmt.Errorf("load session %q: %w", name, err)
	}

	src, err := gitlabapi.New(sess.Source)
	if err != nil {
		return err
	}
	roots, err := src.SourceTree()
	if err != nil {
		return fmt.Errorf("discover source: %w", err)
	}
	byID := map[int64]*gitlabapi.Node{}
	indexNodes(roots, byID)

	selected := map[int64]bool{}
	for _, id := range sess.Selected {
		selected[id] = true
	}
	force := map[int64]bool{}
	for _, id := range sess.Force {
		force[id] = true
	}

	// Resolve effective target namespaces (cascading group assignments) and
	// per-project options.
	targets, unmapped := gitlabapi.ResolveTargets(roots, sess.Assignments, selected)
	optMap := gitlabapi.ResolveOptions(roots, sess.OptionOverrides, sess.Options, selected)
	for _, id := range unmapped {
		fmt.Fprintf(os.Stderr, "warning: selected project id %d has no target namespace (skipped)\n", id)
	}

	var items []migrate.Item
	var missing []int64
	for id := range selected {
		node, ok := byID[id]
		if !ok {
			missing = append(missing, id)
			continue
		}
		ns, ok := targets[id]
		if !ok {
			continue // unmapped (already warned)
		}
		items = append(items, migrate.Item{Node: node, TargetNamespace: ns, Force: force[id], Options: optMap[id]})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Node.FullPath < items[j].Node.FullPath })

	for _, id := range missing {
		fmt.Fprintf(os.Stderr, "warning: selected project id %d not found on source (skipped)\n", id)
	}
	if len(items) == 0 {
		return fmt.Errorf("no projects to migrate")
	}

	workDir, err := os.MkdirTemp("", "gitlab-copy-run-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(workDir)

	// Path map for the URL rewrite: the session's own accumulated map plus an
	// optional explicit file. The external file is used for this run only; it is
	// not persisted back into the session.
	extra := map[string]string{}
	for k, v := range sess.PathMap {
		extra[k] = v
	}
	if pathMapFile != "" {
		more, err := config.LoadPathMapFrom(pathMapFile)
		if err != nil {
			return fmt.Errorf("load --path-map: %w", err)
		}
		for k, v := range more {
			extra[k] = v
		}
	}

	plan := migrate.Plan{
		Source:           sess.Source,
		Target:           sess.Target,
		Options:          sess.Options,
		Items:            items,
		WorkDir:          workDir,
		OldBaseURL:       sess.Source.URL,
		NewBaseURL:       sess.Target.URL,
		ExtraPaths:       extra,
		Roots:            roots,
		LastFingerprints: sess.Transferred,
	}

	if dryRun {
		fmt.Printf("DRY RUN — %d project(s), options: %+v\n", len(items), plan.Options)
		for _, it := range items {
			forced := ""
			if it.Force {
				forced = "  [force]"
			}
			fmt.Printf("  %s → %s/%s%s\n", it.Node.FullPath, it.TargetNamespace, it.Node.Path, forced)
		}
		if len(extra) > 0 {
			fmt.Printf("  (+ %d known path remap(s) for URL rewrite)\n", len(extra))
		}
		return nil
	}

	eng, err := migrate.NewEngine(plan)
	if err != nil {
		return err
	}
	results := eng.Run(context.Background(), plan, func(ev migrate.Event) {
		switch ev.Type {
		case "project_start":
			fmt.Printf("▶ %s\n", ev.Project)
		case "log":
			fmt.Printf("    %s\n", ev.Message)
		case "project_done":
			if ev.Result != nil {
				fmt.Printf("  [%s] %s\n", ev.Result.Status, ev.Project)
			}
		}
	})
	// Record the migrated repos into the session's path map + transfer
	// fingerprints for future runs.
	if sess.PathMap == nil {
		sess.PathMap = map[string]string{}
	}
	for k, v := range migrate.RecordPathMappings(results) {
		sess.PathMap[k] = v
	}
	if sess.Transferred == nil {
		sess.Transferred = map[int64]string{}
	}
	for id, fp := range migrate.RecordTransferred(results) {
		sess.Transferred[id] = fp
	}
	_ = config.Save(sess, time.Now())

	fmt.Println("\nSummary:", migrate.Summary(results))
	for _, r := range results {
		if r.Status == migrate.StatusFailed {
			return fmt.Errorf("one or more projects failed")
		}
	}
	return nil
}

func indexNodes(nodes []*gitlabapi.Node, byID map[int64]*gitlabapi.Node) {
	for _, n := range nodes {
		byID[n.ID] = n
		indexNodes(n.Children, byID)
	}
}
