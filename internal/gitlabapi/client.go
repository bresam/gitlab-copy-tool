// Package gitlabapi wraps the GitLab REST API v4 client for the parts the
// migration tool needs: structure discovery, target namespace listing,
// group-path creation and project lookup/creation.
package gitlabapi

import (
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/bresam/gitlab-copy-tool/internal/config"
	gitlab "gitlab.com/gitlab-org/api/client-go"
)

// Client is a thin wrapper around the GitLab client bound to one instance.
type Client struct {
	GL      *gitlab.Client
	BaseURL string // e.g. https://gitlab.example.com
	Host    string // e.g. gitlab.example.com

	// groupHints maps a group path slug to the source group's display name and
	// visibility, so created/updated target groups match the source exactly
	// (name and path can differ, e.g. name "Public" / path "pub").
	groupHints map[string]GroupInfo
}

// GroupInfo carries a source group's display name and visibility.
type GroupInfo struct {
	Name       string
	Visibility string
}

// TargetVisibility maps a source visibility to the value used on the target.
// GitLab SaaS (gitlab.com) has no "internal" level, and an internal repo must
// never become public, so "internal" maps to "private". Other values pass
// through unchanged.
func TargetVisibility(v string) string {
	if v == "internal" {
		return "private"
	}
	return v
}

// SetGroupHints installs the source-group name/visibility lookup on the client.
func (c *Client) SetGroupHints(h map[string]GroupInfo) { c.groupHints = h }

// BuildGroupHints walks the source tree and returns a slug -> GroupInfo map for
// every group, used to replicate group name and visibility onto the target.
func BuildGroupHints(roots []*Node) map[string]GroupInfo {
	hints := map[string]GroupInfo{}
	var walk func(n *Node)
	walk = func(n *Node) {
		if n.Kind == "group" {
			hints[n.Path] = GroupInfo{Name: n.Name, Visibility: TargetVisibility(n.Visibility)}
		}
		for _, c := range n.Children {
			walk(c)
		}
	}
	for _, r := range roots {
		walk(r)
	}
	return hints
}

// Node is an entry in the source structure tree (a group or a project).
type Node struct {
	ID       int64
	Kind     string // "group" or "project"
	Name     string
	Path     string
	FullPath string
	ParentID int64

	// Visibility is the GitLab visibility ("private"|"internal"|"public") of the
	// group or project, replicated onto the target.
	Visibility string

	// Project-only fields.
	SSHURL        string
	HTTPURL       string
	DefaultBranch string
	NamespacePath string
	EmptyRepo     bool
	HasContainers bool // project has at least one container-registry image

	Children []*Node
}

// Namespace is a target a project can be created in — either a group or the
// user's personal namespace.
type Namespace struct {
	ID       int64
	FullPath string
	Kind     string // "group" or "user"
}

// New builds a Client from an endpoint config, resolving the token.
func New(ep config.Endpoint) (*Client, error) {
	base := strings.TrimRight(strings.TrimSpace(ep.URL), "/")
	if base == "" {
		return nil, fmt.Errorf("endpoint URL is empty")
	}
	u, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("invalid endpoint URL %q: %w", base, err)
	}
	token := config.ResolveToken(ep.Token)
	gl, err := gitlab.NewClient(token, gitlab.WithBaseURL(base+"/api/v4"))
	if err != nil {
		return nil, err
	}
	return &Client{GL: gl, BaseURL: base, Host: u.Host}, nil
}

// Ping verifies connectivity and returns the current user login and version.
func (c *Client) Ping() (login, version string, err error) {
	u, _, err := c.GL.Users.CurrentUser()
	if err != nil {
		return "", "", err
	}
	v, _, err := c.GL.Version.GetVersion()
	if err != nil {
		return u.Username, "", err
	}
	return u.Username, v.Version, nil
}

// SourceTree discovers all accessible groups and their projects and returns
// the top-level nodes of the hierarchy.
func (c *Client) SourceTree() ([]*Node, error) {
	groups, err := c.allGroups(nil)
	if err != nil {
		return nil, err
	}

	byID := make(map[int64]*Node, len(groups))
	for _, g := range groups {
		byID[g.ID] = &Node{
			ID:         g.ID,
			Kind:       "group",
			Name:       g.Name,
			Path:       g.Path,
			FullPath:   g.FullPath,
			ParentID:   g.ParentID,
			Visibility: string(g.Visibility),
		}
	}

	// Attach projects to their group.
	for _, g := range groups {
		projects, err := c.groupProjects(g.ID)
		if err != nil {
			return nil, fmt.Errorf("list projects of group %s: %w", g.FullPath, err)
		}
		parent := byID[g.ID]
		for _, p := range projects {
			n := projectNode(p)
			// Cheap pre-filter (ContainerRegistryEnabled) before the extra call.
			if p.ContainerRegistryEnabled {
				n.HasContainers = c.hasContainerImages(p.ID)
			}
			parent.Children = append(parent.Children, n)
		}
	}

	// Build the group hierarchy.
	var roots []*Node
	for _, g := range groups {
		n := byID[g.ID]
		if g.ParentID != 0 {
			if p, ok := byID[g.ParentID]; ok {
				p.Children = append(p.Children, n)
				continue
			}
		}
		roots = append(roots, n)
	}

	// Personal-namespace projects are intentionally excluded: only group
	// projects are migrated.

	sortTree(roots)
	return roots, nil
}

func projectNode(p *gitlab.Project) *Node {
	ns := ""
	if p.Namespace != nil {
		ns = p.Namespace.FullPath
	}
	return &Node{
		ID:            p.ID,
		Kind:          "project",
		Name:          p.Name,
		Path:          p.Path,
		FullPath:      p.PathWithNamespace,
		Visibility:    string(p.Visibility),
		SSHURL:        p.SSHURLToRepo,
		HTTPURL:       p.HTTPURLToRepo,
		DefaultBranch: p.DefaultBranch,
		NamespacePath: ns,
		EmptyRepo:     p.EmptyRepo,
	}
}

func sortTree(nodes []*Node) {
	sort.SliceStable(nodes, func(i, j int) bool {
		if nodes[i].Kind != nodes[j].Kind {
			return nodes[i].Kind == "group" // groups before projects
		}
		return strings.ToLower(nodes[i].Name) < strings.ToLower(nodes[j].Name)
	})
	for _, n := range nodes {
		sortTree(n.Children)
	}
}

func (c *Client) allGroups(minAccess *gitlab.AccessLevelValue) ([]*gitlab.Group, error) {
	opt := &gitlab.ListGroupsOptions{
		ListOptions:  gitlab.ListOptions{PerPage: 100},
		AllAvailable: gitlab.Ptr(true),
	}
	if minAccess != nil {
		opt.MinAccessLevel = minAccess
	}
	var all []*gitlab.Group
	for {
		groups, resp, err := c.GL.Groups.ListGroups(opt)
		if err != nil {
			return nil, err
		}
		all = append(all, groups...)
		if resp.NextPage == 0 {
			break
		}
		opt.Page = resp.NextPage
	}
	return all, nil
}

func (c *Client) groupProjects(gid int64) ([]*gitlab.Project, error) {
	opt := &gitlab.ListGroupProjectsOptions{
		ListOptions:      gitlab.ListOptions{PerPage: 100},
		IncludeSubGroups: gitlab.Ptr(false),
		Archived:         gitlab.Ptr(false),
	}
	var all []*gitlab.Project
	for {
		projects, resp, err := c.GL.Groups.ListGroupProjects(gid, opt)
		if err != nil {
			return nil, err
		}
		all = append(all, projects...)
		if resp.NextPage == 0 {
			break
		}
		opt.Page = resp.NextPage
	}
	return all, nil
}

// TargetNamespaces lists the groups the user can create projects in (at least
// Maintainer access). Personal namespaces are intentionally excluded.
func (c *Client) TargetNamespaces() ([]Namespace, error) {
	groups, err := c.allGroups(gitlab.Ptr(gitlab.MaintainerPermissions))
	if err != nil {
		return nil, err
	}
	out := make([]Namespace, 0, len(groups))
	for _, g := range groups {
		out = append(out, Namespace{ID: g.ID, FullPath: g.FullPath, Kind: "group"})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].FullPath < out[j].FullPath })
	return out, nil
}

// parentPath returns everything before the last path segment ("" for a
// top-level path).
func parentPath(p string) string {
	p = strings.Trim(p, "/")
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[:i]
	}
	return ""
}

// ResolveTargets computes the effective target namespace for each selected
// project. assignments maps a node ID (group OR project) to a chosen base
// target namespace full path. A group assignment cascades to all descendants
// while preserving the sub-structure (e.g. assigning group "tools" to base
// "example-org" puts a repo in "tools/ci" at "example-org/tools/ci").
// The nearest assignment (project > subgroup > group) wins. It returns
// projectID -> namespace and the IDs of selected projects with no target.
func ResolveTargets(roots []*Node, assignments map[int64]string, selected map[int64]bool) (map[int64]string, []int64) {
	type inherited struct {
		base         string
		parentPrefix string // parentPath of the assigned group's full path
	}
	targets := map[int64]string{}
	var unmapped []int64

	var walk func(n *Node, in *inherited)
	walk = func(n *Node, in *inherited) {
		if n.Kind == "group" {
			cur := in
			if base, ok := assignments[n.ID]; ok {
				cur = &inherited{base: base, parentPrefix: parentPath(n.FullPath)}
			}
			for _, c := range n.Children {
				walk(c, cur)
			}
			return
		}
		// project
		if !selected[n.ID] {
			return
		}
		if base, ok := assignments[n.ID]; ok {
			targets[n.ID] = base // explicit per-repo target
			return
		}
		if in != nil {
			rel := strings.Trim(strings.TrimPrefix(n.NamespacePath, in.parentPrefix), "/")
			ns := in.base
			if rel != "" {
				ns = in.base + "/" + rel
			}
			targets[n.ID] = ns
			return
		}
		unmapped = append(unmapped, n.ID)
	}
	for _, r := range roots {
		walk(r, nil)
	}
	return targets, unmapped
}

// visPtr converts a visibility string to a *VisibilityValue (nil when empty).
func visPtr(v string) *gitlab.VisibilityValue {
	if v == "" {
		return nil
	}
	x := gitlab.VisibilityValue(v)
	return &x
}

// ResolveOptions computes the effective option set for each selected project by
// walking the tree top-down: each node's overrides are applied on top of the
// inherited set, so the nearest override (project > subgroup > group) wins and
// the baseline applies where nothing is set. overrides maps node ID -> option
// index -> value.
func ResolveOptions(roots []*Node, overrides map[int64]map[int]bool, baseline config.Options, selected map[int64]bool) map[int64]config.Options {
	out := map[int64]config.Options{}
	var walk func(n *Node, eff config.Options)
	walk = func(n *Node, eff config.Options) {
		if ov, ok := overrides[n.ID]; ok {
			for idx, v := range ov {
				eff = eff.With(idx, v)
			}
		}
		if n.Kind == "project" {
			if selected[n.ID] {
				out[n.ID] = eff
			}
			return
		}
		for _, c := range n.Children {
			walk(c, eff)
		}
	}
	for _, r := range roots {
		walk(r, baseline)
	}
	return out
}

// EffectiveOptionsForNode returns the effective options at a single node (group
// or project), i.e. the baseline plus every override along the path down to it.
// Used by the UI to display a node's current settings.
func EffectiveOptionsForNode(roots []*Node, overrides map[int64]map[int]bool, baseline config.Options, nodeID int64) config.Options {
	var found *config.Options
	var walk func(n *Node, eff config.Options)
	walk = func(n *Node, eff config.Options) {
		if ov, ok := overrides[n.ID]; ok {
			for idx, v := range ov {
				eff = eff.With(idx, v)
			}
		}
		if n.ID == nodeID {
			e := eff
			found = &e
			return
		}
		for _, c := range n.Children {
			if found == nil {
				walk(c, eff)
			}
		}
	}
	for _, r := range roots {
		if found == nil {
			walk(r, baseline)
		}
	}
	if found != nil {
		return *found
	}
	return baseline
}

// EnsureGroupPath makes sure the full group path exists on the target and
// returns the leaf group ID. It walks the path top-down: missing (sub)groups
// are created with the requested visibility (falling back to private if that is
// rejected), and existing groups are upgraded to the requested visibility when
// it is more visible than their current one (never downgraded). This lets a
// re-run repair visibility that had to fall back on an earlier run.
func (c *Client) EnsureGroupPath(fullPath, visibility string) (int64, error) {
	fullPath = strings.Trim(fullPath, "/")
	if fullPath == "" {
		return 0, fmt.Errorf("empty group path")
	}

	segments := strings.Split(fullPath, "/")
	var parentID int64
	var accum string
	for i, seg := range segments {
		if i == 0 {
			accum = seg
		} else {
			accum = accum + "/" + seg
		}
		name, vis := c.groupSpec(seg, visibility)
		if g, _, err := c.GL.Groups.GetGroup(accum, nil); err == nil {
			c.reconcileGroup(g, name, vis)
			parentID = g.ID
			continue
		}
		id, err := c.createGroup(seg, name, parentID, vis)
		if err != nil {
			return 0, fmt.Errorf("create group %q: %w", accum, err)
		}
		parentID = id
	}
	return parentID, nil
}

// groupSpec returns the desired display name and visibility for a group path
// segment, preferring the source group's values (from the hints) and falling
// back to the slug as the name and the given fallback visibility.
func (c *Client) groupSpec(seg, fallbackVis string) (name, visibility string) {
	name, visibility = seg, fallbackVis
	if info, ok := c.groupHints[seg]; ok {
		if info.Name != "" {
			name = info.Name
		}
		if info.Visibility != "" {
			visibility = info.Visibility
		}
	}
	return name, visibility
}

func visRank(v string) int {
	switch v {
	case "public":
		return 2
	case "internal":
		return 1
	default:
		return 0 // private / unknown
	}
}

// reconcileGroup aligns an existing target group with the source: it renames it
// to the source display name and raises its visibility (never lowers). Best-
// effort: failures (permissions, parent constraints) are ignored so the
// migration proceeds and the project step reports any remaining problem.
func (c *Client) reconcileGroup(g *gitlab.Group, name, desiredVis string) {
	opt := &gitlab.UpdateGroupOptions{}
	changed := false
	if desiredVis != "" && visRank(desiredVis) > visRank(string(g.Visibility)) {
		opt.Visibility = visPtr(desiredVis)
		changed = true
	}
	if name != "" && name != g.Name {
		opt.Name = gitlab.Ptr(name)
		changed = true
	}
	if changed {
		_, _, _ = c.GL.Groups.UpdateGroup(g.ID, opt)
	}
}

func (c *Client) createGroup(seg, name string, parentID int64, visibility string) (int64, error) {
	mk := func(vis string) (*gitlab.Group, error) {
		opt := &gitlab.CreateGroupOptions{
			Name:       gitlab.Ptr(name),
			Path:       gitlab.Ptr(seg),
			Visibility: visPtr(vis),
		}
		if parentID != 0 {
			opt.ParentID = gitlab.Ptr(parentID)
		}
		g, _, err := c.GL.Groups.CreateGroup(opt)
		return g, err
	}
	g, err := mk(visibility)
	if err != nil && visibility != "" && visibility != "private" {
		// Visibility not allowed here (e.g. public under private parent):
		// fall back to private so the structure still gets created.
		g, err = mk("private")
	}
	if err != nil {
		return 0, err
	}
	return g.ID, nil
}

// DeleteProjectAndWait deletes the project at fullPath and waits until it is
// gone, so it can be recreated cleanly. GitLab deletion is two-stage: the first
// delete marks the project "inactive" and renames it to
// "<path>-deletion_scheduled-<id>"; a second delete with permanently_remove and
// the project's *current* full path removes it for good. logf reports progress.
func (c *Client) DeleteProjectAndWait(fullPath string, timeout time.Duration, logf func(string, ...any)) error {
	p, resp, err := c.GL.Projects.GetProject(fullPath, nil)
	if err != nil {
		if resp != nil && resp.StatusCode == 404 {
			return nil // nothing to delete
		}
		return err
	}
	id := p.ID

	// Stage 1: mark for deletion (moves it to "inactive").
	if _, err := c.GL.Projects.DeleteProject(id, nil); err != nil {
		return fmt.Errorf("delete (stage 1): %w", err)
	}

	deadline := time.Now().Add(timeout)
	permTried := false
	for {
		cur, resp, err := c.GL.Projects.GetProject(id, nil)
		if err != nil {
			if resp != nil && resp.StatusCode == 404 {
				return nil // permanently gone
			}
			// transient error — keep waiting
		} else if cur.MarkedForDeletionOn != nil && !permTried {
			// Stage 2: permanently remove the now-inactive project. full_path
			// must be its *current* (renamed) path.
			permTried = true
			if _, err := c.GL.Projects.DeleteProject(id, &gitlab.DeleteProjectOptions{
				PermanentlyRemove: gitlab.Ptr(true),
				FullPath:          gitlab.Ptr(cur.PathWithNamespace),
			}); err != nil && logf != nil {
				logf("force: permanent removal request failed: %v", err)
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("target %q not fully deleted within %s — remove the inactive project manually and retry", fullPath, timeout)
		}
		if logf != nil {
			logf("force: waiting for permanent deletion…")
		}
		time.Sleep(2 * time.Second)
	}
}

// SetProjectVisibility sets a project's visibility (private/internal/public).
func (c *Client) SetProjectVisibility(pid int64, visibility string) error {
	_, _, err := c.GL.Projects.EditProject(pid, &gitlab.EditProjectOptions{
		Visibility: visPtr(visibility),
	})
	return err
}

// FindProject returns the live project at the given full path, or nil if absent.
// A project that is pending deletion (gitlab.com renames it to
// "<path>-deletion_scheduled-<id>" and keeps a redirect from the old path) is
// treated as absent, so the path counts as free/creatable again.
func (c *Client) FindProject(fullPath string) (*gitlab.Project, error) {
	p, resp, err := c.GL.Projects.GetProject(fullPath, nil)
	if err != nil {
		if resp != nil && resp.StatusCode == 404 {
			return nil, nil
		}
		return nil, err
	}
	if p.MarkedForDeletionOn != nil {
		return nil, nil // pending deletion → path effectively free
	}
	if want := strings.Trim(fullPath, "/"); !strings.EqualFold(p.PathWithNamespace, want) {
		return nil, nil // reached via a redirect from a renamed project
	}
	return p, nil
}

// EnsureProject returns an existing project at namespacePath/path or creates
// it with the given visibility. Created target groups get the same visibility.
// If the requested visibility is rejected (e.g. public under a private parent)
// the project is created with the default visibility instead so the migration
// can proceed. The bool reports whether the project already existed.
func (c *Client) EnsureProject(namespacePath, path, name, visibility string) (*gitlab.Project, bool, error) {
	full := namespacePath + "/" + path

	// Ensure/upgrade the namespace groups first — also on re-runs where the
	// project already exists — so a public/internal project can live here.
	nsID, err := c.ResolveNamespaceID(namespacePath, visibility)
	if err != nil {
		return nil, false, err
	}

	if p, err := c.FindProject(full); err != nil {
		return nil, false, err
	} else if p != nil {
		return p, true, nil
	}

	mk := func(vis string) (*gitlab.Project, error) {
		opt := &gitlab.CreateProjectOptions{
			Name:        gitlab.Ptr(name),
			Path:        gitlab.Ptr(path),
			NamespaceID: gitlab.Ptr(nsID),
			Visibility:  visPtr(vis),
		}
		p, _, err := c.GL.Projects.CreateProject(opt)
		return p, err
	}
	p, err := mk(visibility)
	if err != nil && visibility != "" && visibility != "private" {
		p, err = mk("") // fall back to the namespace default
	}
	if err != nil {
		return nil, false, fmt.Errorf("create project %q: %w", full, err)
	}
	return p, false, nil
}

// ResolveNamespaceID returns the leaf group ID for a target namespace path,
// creating missing (sub)groups and upgrading existing ones to the requested
// visibility as needed. Target namespaces are always groups (personal
// namespaces are excluded from the migration).
func (c *Client) ResolveNamespaceID(path, visibility string) (int64, error) {
	return c.EnsureGroupPath(path, visibility)
}
