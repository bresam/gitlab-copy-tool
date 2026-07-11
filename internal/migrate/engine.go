// Package migrate orchestrates a migration run: for each selected project it
// ensures the target group path and project, mirrors the repository, and then
// runs the optional (failsafe) steps — URL rewrite and metadata copy.
//
// Failure policy: only the hard steps (group path, project creation, repo
// mirror) can mark a project as failed. The optional steps only ever add
// warnings; the project still counts as a success.
package migrate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/bresam/gitlab-copy-tool/internal/config"
	"github.com/bresam/gitlab-copy-tool/internal/gitlabapi"
	"github.com/bresam/gitlab-copy-tool/internal/gittransport"
	"github.com/bresam/gitlab-copy-tool/internal/rewrite"
	gitlab "gitlab.com/gitlab-org/api/client-go"
)

// Status of a single project migration.
type Status string

const (
	StatusOK      Status = "ok"
	StatusWarn    Status = "warn"
	StatusSkipped Status = "skipped"
	StatusFailed  Status = "failed"
)

// Item is one project to migrate together with its resolved target namespace.
type Item struct {
	Node            *gitlabapi.Node
	TargetNamespace string
	// Force overwrites the target repo even if it holds newer/divergent history
	// (disables the existing-target guard for this project only).
	Force bool
}

// Plan is the full set of work for a run.
type Plan struct {
	Source     config.Endpoint
	Target     config.Endpoint
	Options    config.Options
	Items      []Item
	WorkDir    string
	OldBaseURL string
	NewBaseURL string
	// Roots is the discovered source tree, used to replicate group name and
	// visibility onto created target groups.
	Roots []*gitlabapi.Node
	// ExtraPaths carries old->new namespace/path remappings from earlier runs
	// (loaded from the cumulative path map) so the URL rewrite can also fix
	// references to repos migrated in previous sessions.
	ExtraPaths map[string]string
}

// ProjectResult captures the outcome for one project.
type ProjectResult struct {
	Source   string
	Target   string
	Status   Status
	Reason   string
	Warnings []string
}

// Event is emitted during a run for live UI feedback.
type Event struct {
	Type    string // "log" | "project_start" | "project_done"
	Project string
	Message string
	Result  *ProjectResult
}

// Engine holds the connected clients for a run.
type Engine struct {
	src *gitlabapi.Client
	tgt *gitlabapi.Client
}

// NewEngine connects to both instances.
func NewEngine(plan Plan) (*Engine, error) {
	src, err := gitlabapi.New(plan.Source)
	if err != nil {
		return nil, fmt.Errorf("source client: %w", err)
	}
	tgt, err := gitlabapi.New(plan.Target)
	if err != nil {
		return nil, fmt.Errorf("target client: %w", err)
	}
	tgt.SetGroupHints(gitlabapi.BuildGroupHints(plan.Roots))
	return &Engine{src: src, tgt: tgt}, nil
}

// Run executes the plan, emitting events. It returns all project results.
func (e *Engine) Run(ctx context.Context, plan Plan, emit func(Event)) []ProjectResult {
	if emit == nil {
		emit = func(Event) {}
	}
	results := make([]ProjectResult, 0, len(plan.Items))
	for _, item := range plan.Items {
		if ctx.Err() != nil {
			break
		}
		emit(Event{Type: "project_start", Project: item.Node.FullPath})
		res := e.migrateOne(ctx, plan, item, emit)
		results = append(results, res)
		r := res
		emit(Event{Type: "project_done", Project: res.Source, Result: &r})
	}
	return results
}

func (e *Engine) migrateOne(ctx context.Context, plan Plan, item Item, emit func(Event)) ProjectResult {
	node := item.Node
	targetFull := item.TargetNamespace + "/" + node.Path
	res := ProjectResult{Source: node.FullPath, Target: targetFull, Status: StatusOK}
	logf := func(format string, args ...any) {
		emit(Event{Type: "log", Project: node.FullPath, Message: fmt.Sprintf(format, args...)})
	}
	fail := func(err error) ProjectResult {
		res.Status = StatusFailed
		res.Reason = err.Error()
		logf("FAILED: %v", err)
		return res
	}
	warn := func(context string, err error) {
		res.Warnings = append(res.Warnings, fmt.Sprintf("%s: %v", context, err))
		logf("warning: %s: %v", context, err)
	}

	// Target visibility: source "internal" has no SaaS equivalent and must not
	// become public — it maps to "private".
	wantVis := gitlabapi.TargetVisibility(node.Visibility)

	// 1. Ensure the target project exists (resolving/creating the namespace —
	//    group path or personal namespace). (hard)
	tgtProj, existed, err := e.tgt.EnsureProject(item.TargetNamespace, node.Path, node.Name, wantVis)
	if err != nil {
		return fail(err)
	}
	if existed {
		logf("target project already exists")
	} else {
		logf("created target project %s", targetFull)
	}

	// Reconcile repo visibility to match the source on every run (failsafe:
	// warns if e.g. public is not allowed under a private target namespace).
	if wantVis != "" && string(tgtProj.Visibility) != wantVis {
		if err := e.tgt.SetProjectVisibility(tgtProj.ID, wantVis); err != nil {
			warn("visibility", err)
		} else {
			logf("visibility set to %s", wantVis)
			tgtProj.Visibility = gitlab.VisibilityValue(wantVis)
		}
	}

	// 3. Mirror the repository. (hard)
	if node.EmptyRepo {
		logf("source repository is empty, nothing to mirror")
		warn("mirror", fmt.Errorf("source repo empty"))
	} else {
		workDir, err := os.MkdirTemp(plan.WorkDir, "gct-")
		if err != nil {
			return fail(err)
		}
		defer os.RemoveAll(workDir)

		spec := gittransport.Spec{
			Source: gittransport.Repo{
				SSHURL:    node.SSHURL,
				HTTPURL:   node.HTTPURL,
				Token:     config.ResolveToken(plan.Source.Token),
				Transport: plan.Source.Transport,
			},
			Target: gittransport.Repo{
				SSHURL:    tgtProj.SSHURLToRepo,
				HTTPURL:   tgtProj.HTTPURLToRepo,
				Token:     config.ResolveToken(plan.Target.Token),
				Transport: plan.Target.Transport,
			},
			WorkDir: workDir,
			Force:   item.Force,
		}
		mres, err := gittransport.Mirror(spec, logf)
		if err != nil {
			return fail(fmt.Errorf("mirror: %w", err))
		}
		if mres.Skipped {
			res.Status = StatusSkipped
			res.Reason = mres.Reason
			logf("skipped: %s", mres.Reason)
			return res
		}
		if mres.Forced {
			warn("force-overwrite", fmt.Errorf("overwrote target that had newer content (%s)", mres.Reason))
		}
		logf("repository mirrored")

		// 4. URL rewrite. (optional, failsafe)
		if plan.Options.URLRewrite && plan.OldBaseURL != "" && plan.NewBaseURL != "" {
			if err := e.rewriteAndCommit(plan, item, tgtProj, mres.TargetURL, workDir, logf); err != nil {
				warn("url-rewrite", err)
			}
		}
	}

	// 5. Metadata copy. (optional, failsafe)
	e.copyMetadata(plan, node, tgtProj, warn, logf)

	if len(res.Warnings) > 0 && res.Status == StatusOK {
		res.Status = StatusWarn
	}
	return res
}

func (e *Engine) rewriteAndCommit(plan Plan, item Item, tgtProj *gitlab.Project, targetURL, workDir string, logf gittransport.Logf) error {
	node := item.Node
	branch := node.DefaultBranch
	if branch == "" {
		branch = tgtProj.DefaultBranch
	}
	if branch == "" {
		return fmt.Errorf("no default branch to rewrite")
	}
	wt := filepath.Join(workDir, "rewrite")
	if err := gittransport.CheckoutBranch(targetURL, branch, wt); err != nil {
		return err
	}
	changes, err := rewrite.Run(wt, rewrite.Options{
		OldURL: plan.OldBaseURL,
		NewURL: plan.NewBaseURL,
		Paths:  pathMappings(plan),
		Prefix: firstSegment(item.TargetNamespace),
	})
	if err != nil {
		return err
	}
	if len(changes) == 0 {
		logf("url-rewrite: no occurrences found")
		return nil
	}
	msg := fmt.Sprintf("chore: rewrite GitLab URL %s -> %s (%d files)", plan.OldBaseURL, plan.NewBaseURL, len(changes))
	committed, err := gittransport.CommitAll(wt, msg, gittransport.CommitIdentity{Name: "gitlab-copy-tool"})
	if err != nil {
		return err
	}
	if !committed {
		return nil
	}
	if err := gittransport.PushBranch(wt, targetURL, branch); err != nil {
		return err
	}
	logf("url-rewrite: committed changes in %d file(s) on %s", len(changes), branch)
	return nil
}

// firstSegment returns the first path segment of a namespace path, i.e. the
// account/top-level group (e.g. "example-org/tools" -> "example-org").
func firstSegment(nsPath string) string {
	nsPath = strings.Trim(nsPath, "/")
	if i := strings.IndexByte(nsPath, '/'); i >= 0 {
		return nsPath[:i]
	}
	return nsPath
}

// pathMappings builds the old→new namespace/path remapping used by the rewrite.
// It merges the mappings from earlier runs (ExtraPaths) with those of the
// current run; the current run wins on any conflict.
func pathMappings(plan Plan) []rewrite.PathMapping {
	merged := map[string]string{}
	for old, neu := range plan.ExtraPaths {
		merged[old] = neu
	}
	for _, it := range plan.Items {
		merged[it.Node.FullPath] = it.TargetNamespace + "/" + it.Node.Path
	}
	out := make([]rewrite.PathMapping, 0, len(merged))
	for old, neu := range merged {
		out = append(out, rewrite.PathMapping{OldPath: old, NewPath: neu})
	}
	return out
}

// RecordPathMappings returns the old→new pairs for the successfully migrated
// projects (ok/warn), for persisting into the cumulative path map.
func RecordPathMappings(results []ProjectResult) map[string]string {
	out := map[string]string{}
	for _, r := range results {
		if r.Status == StatusOK || r.Status == StatusWarn {
			out[r.Source] = r.Target
		}
	}
	return out
}

func (e *Engine) copyMetadata(plan Plan, node *gitlabapi.Node, tgtProj *gitlab.Project, warn func(string, error), logf gittransport.Logf) {
	if plan.Options.Issues {
		if err := gitlabapi.CopyLabels(e.src, e.tgt, node.ID, tgtProj.ID); err != nil {
			warn("labels", err)
		}
		if err := gitlabapi.CopyMilestones(e.src, e.tgt, node.ID, tgtProj.ID); err != nil {
			warn("milestones", err)
		}
		if err := gitlabapi.CopyIssues(e.src, e.tgt, node.ID, tgtProj.ID); err != nil {
			warn("issues", err)
		} else {
			logf("issues copied")
		}
		if err := gitlabapi.CopyMergeRequests(e.src, e.tgt, node.ID, tgtProj.ID); err != nil {
			warn("merge-requests", err)
		} else {
			logf("merge requests copied")
		}
	}
	if plan.Options.CIVariables {
		if err := gitlabapi.CopyCIVariables(e.src, e.tgt, node.ID, tgtProj.ID); err != nil {
			warn("ci-variables", err)
		} else {
			logf("CI variables copied")
		}
	}
	if plan.Options.Settings {
		srcProj, _, err := e.src.GL.Projects.GetProject(node.ID, nil)
		if err != nil {
			warn("settings", err)
			return
		}
		if err := gitlabapi.CopySettings(srcProj, e.tgt, tgtProj.ID); err != nil {
			warn("settings", err)
		} else {
			logf("settings copied")
		}
	}
}

// Summary aggregates results into counts by status.
func Summary(results []ProjectResult) string {
	var ok, warn, skip, fail int
	for _, r := range results {
		switch r.Status {
		case StatusOK:
			ok++
		case StatusWarn:
			warn++
		case StatusSkipped:
			skip++
		case StatusFailed:
			fail++
		}
	}
	return strings.TrimSpace(fmt.Sprintf("ok=%d warn=%d skipped=%d failed=%d", ok, warn, skip, fail))
}
