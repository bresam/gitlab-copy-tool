// Package config handles persistence of migration sessions.
//
// Sessions are stored as JSON files under ~/.config/gitlab-copy-tool/sessions/.
// Tokens may be stored inline (file mode 0600) or referenced from the
// environment via a "${ENV_VAR}" placeholder that is resolved at runtime.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Transport selects the git transport used for clone/push.
const (
	TransportAuto  = "auto"
	TransportSSH   = "ssh"
	TransportHTTPS = "https"
)

// Endpoint describes a single GitLab instance (source or target).
type Endpoint struct {
	// URL is the instance base URL without the /api/v4 suffix,
	// e.g. "https://gitlab.example.com".
	URL string `json:"url"`
	// Token is a personal access token, or a "${ENV_VAR}" reference.
	Token string `json:"token"`
	// Transport is one of auto|ssh|https.
	Transport string `json:"transport"`
}

// Options toggles the optional (failsafe) migration steps.
type Options struct {
	Issues            bool `json:"issues"`
	CIVariables       bool `json:"ci_variables"`
	Settings          bool `json:"settings"`
	URLRewrite        bool `json:"url_rewrite"`
	Releases          bool `json:"releases"`
	ContainerRegistry bool `json:"container_registry"`
}

// Option indices (order shown in the UI as keys 1-6).
const (
	OptIssues = iota
	OptCIVariables
	OptSettings
	OptURLRewrite
	OptReleases
	OptContainerRegistry
	NumOptions
)

// OptionLabels are the short labels for each option index.
var OptionLabels = [NumOptions]string{
	"Issues/MRs", "CI-Vars", "Settings", "URL-Rewrite", "Releases", "Container-Registry",
}

// Get returns the option value at index i.
func (o Options) Get(i int) bool {
	switch i {
	case OptIssues:
		return o.Issues
	case OptCIVariables:
		return o.CIVariables
	case OptSettings:
		return o.Settings
	case OptURLRewrite:
		return o.URLRewrite
	case OptReleases:
		return o.Releases
	case OptContainerRegistry:
		return o.ContainerRegistry
	}
	return false
}

// With returns a copy of o with option index i set to v.
func (o Options) With(i int, v bool) Options {
	switch i {
	case OptIssues:
		o.Issues = v
	case OptCIVariables:
		o.CIVariables = v
	case OptSettings:
		o.Settings = v
	case OptURLRewrite:
		o.URLRewrite = v
	case OptReleases:
		o.Releases = v
	case OptContainerRegistry:
		o.ContainerRegistry = v
	}
	return o
}

// Session is a persisted migration configuration.
type Session struct {
	Name   string   `json:"name"`
	Source Endpoint `json:"source"`
	Target Endpoint `json:"target"`

	// Selected holds the source project IDs marked for processing.
	Selected []int64 `json:"selected"`
	// Assignments maps a source node ID (group OR project) to a chosen base
	// target namespace full path. Group assignments cascade to descendants
	// (see gitlabapi.ResolveTargets); nearest assignment wins.
	Assignments map[int64]string `json:"assignments"`

	// Force lists project IDs whose target repo should be overwritten even if it
	// holds newer/divergent history (per-project guard override).
	Force []int64 `json:"force"`

	// PathMap is the cumulative old->new namespace/path map for this session,
	// grown on every successful run and used by the URL rewrite.
	PathMap map[string]string `json:"path_map"`

	// Options is the baseline (run-wide default) for the optional steps.
	Options Options `json:"options"`
	// OptionOverrides holds per-node option overrides: node ID -> option index
	// -> value. A group override cascades to descendants; the nearest override
	// (project > subgroup > group) wins, else the Options baseline applies.
	OptionOverrides map[int64]map[int]bool `json:"option_overrides"`
	// Transferred records, per successfully migrated project ID, a fingerprint
	// of the effective config it was transferred with. A repo is skipped on the
	// next run when this fingerprint is unchanged and the source has no new
	// content.
	Transferred map[int64]string `json:"transferred"`

	UpdatedAt string `json:"updated_at"`
}

// ClearState resets the run-specific state (selection, target assignments,
// force flags and accumulated path map) while keeping the endpoints, tokens,
// options and name.
func (s *Session) ClearState() {
	s.Selected = nil
	s.Assignments = map[int64]string{}
	s.Force = nil
	s.PathMap = map[string]string{}
	s.OptionOverrides = map[int64]map[int]bool{}
	s.Transferred = map[int64]string{}
}

var envRef = regexp.MustCompile(`^\$\{([A-Za-z_][A-Za-z0-9_]*)\}$`)

// ResolveToken returns the effective token, resolving a "${ENV_VAR}"
// reference against the environment when present.
func ResolveToken(raw string) string {
	if m := envRef.FindStringSubmatch(strings.TrimSpace(raw)); m != nil {
		return os.Getenv(m[1])
	}
	return raw
}

// Dir returns the sessions directory, creating it if necessary.
func Dir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		home, herr := os.UserHomeDir()
		if herr != nil {
			return "", err
		}
		base = filepath.Join(home, ".config")
	}
	dir := filepath.Join(base, "gitlab-copy-tool", "sessions")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

func pathFor(name string) (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, sanitize(name)+".json"), nil
}

func sanitize(name string) string {
	name = strings.TrimSpace(name)
	repl := strings.NewReplacer("/", "_", "\\", "_", " ", "-", ":", "_")
	return repl.Replace(name)
}

// List returns the names of all saved sessions, sorted.
func List() ([]string, error) {
	dir, err := Dir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		names = append(names, strings.TrimSuffix(e.Name(), ".json"))
	}
	sort.Strings(names)
	return names, nil
}

// Load reads a session by name.
func Load(name string) (*Session, error) {
	p, err := pathFor(name)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var s Session
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parse session %q: %w", name, err)
	}
	if s.Assignments == nil {
		s.Assignments = map[int64]string{}
	}
	if s.PathMap == nil {
		s.PathMap = map[string]string{}
	}
	if s.OptionOverrides == nil {
		s.OptionOverrides = map[int64]map[int]bool{}
	}
	if s.Transferred == nil {
		s.Transferred = map[int64]string{}
	}
	return &s, nil
}

// Save writes a session, stamping UpdatedAt with the provided time.
func Save(s *Session, now time.Time) error {
	if strings.TrimSpace(s.Name) == "" {
		return fmt.Errorf("session name is required")
	}
	if s.Assignments == nil {
		s.Assignments = map[int64]string{}
	}
	if s.PathMap == nil {
		s.PathMap = map[string]string{}
	}
	if s.OptionOverrides == nil {
		s.OptionOverrides = map[int64]map[int]bool{}
	}
	if s.Transferred == nil {
		s.Transferred = map[int64]string{}
	}
	s.UpdatedAt = now.UTC().Format(time.RFC3339)
	p, err := pathFor(s.Name)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, data, 0o600)
}

// Remove deletes a saved session.
func Remove(name string) error {
	p, err := pathFor(name)
	if err != nil {
		return err
	}
	return os.Remove(p)
}
