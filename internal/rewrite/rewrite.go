// Package rewrite updates references to the old GitLab host in a checked-out
// working tree: in every composer.json (at any depth) and in every regular file
// located directly in the repository root.
//
// It handles all URL variants that can point at a GitLab server — https/http
// (`//host/…`), scp-like SSH (`git@host:…`) and ssh:// (`ssh://git@host/…`) —
// and keeps the original form. For each reference it:
//   - looks up the repo path in the migration path map (exact) and, if found,
//     rewrites host + full new path (which already includes the account prefix);
//   - otherwise swaps only the host and, if an account prefix is configured,
//     prepends it to the path (unless already present).
package rewrite

import (
	"bytes"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Change records one rewritten file.
type Change struct {
	Path        string // path relative to root
	Occurrences int
}

// PathMapping maps a migrated repo's old namespace/path to its new one.
type PathMapping struct {
	OldPath string // e.g. "old-group/lib"
	NewPath string // e.g. "example-org/new-group/lib"
}

// Options configures the rewrite. OldURL and NewURL are the instance base URLs
// (only their hosts are used). Paths carries the per-repo path remapping for
// repos that are part of the migration. Prefix is the target account/top-level
// namespace prepended to references that are not in Paths (host-only rewrites).
type Options struct {
	OldURL string
	NewURL string
	Paths  []PathMapping
	Prefix string
	// MaxFileSize skips files larger than this many bytes (0 = 5 MiB default).
	MaxFileSize int64
}

const defaultMaxFileSize = 5 << 20 // 5 MiB

// hostOf extracts the host (with optional port) from a URL or bare host string.
func hostOf(s string) string {
	s = strings.TrimSpace(s)
	if u, err := url.Parse(s); err == nil && u.Host != "" {
		return u.Host
	}
	return strings.TrimRight(strings.TrimPrefix(strings.TrimPrefix(s, "https://"), "http://"), "/")
}

func trimPath(p string) string { return strings.Trim(strings.TrimSpace(p), "/") }

// rewriter holds the compiled replacement logic for one Run.
type rewriter struct {
	oldHost string
	newHost string
	prefix  string
	paths   map[string]string // old core path -> new core path
	re      *regexp.Regexp
	bare    *strings.Replacer
}

func newRewriter(oldHost, newHost, prefix string, pms []PathMapping) *rewriter {
	paths := make(map[string]string, len(pms))
	for _, p := range pms {
		op := trimPath(p.OldPath)
		if op == "" {
			continue
		}
		paths[op] = trimPath(p.NewPath)
	}
	// Match host + separator (`/` for URLs, `:` for scp) + path token.
	re := regexp.MustCompile(`(//|@)` + regexp.QuoteMeta(oldHost) + `([:/])([A-Za-z0-9._~\-/]+)`)
	return &rewriter{
		oldHost: oldHost,
		newHost: newHost,
		prefix:  trimPath(prefix),
		paths:   paths,
		re:      re,
		bare: strings.NewReplacer(
			"//"+oldHost, "//"+newHost,
			"@"+oldHost, "@"+newHost,
		),
	}
}

// apply rewrites all references in content.
func (r *rewriter) apply(content string) string {
	out := r.re.ReplaceAllStringFunc(content, func(m string) string {
		g := r.re.FindStringSubmatch(m)
		pre, sep, path := g[1], g[2], g[3]
		core, gitSuffix := splitGit(path)
		if np, ok := r.paths[core]; ok {
			return pre + r.newHost + sep + np + gitSuffix
		}
		if r.prefix != "" && core != r.prefix && !strings.HasPrefix(core, r.prefix+"/") {
			core = r.prefix + "/" + core
		}
		return pre + r.newHost + sep + core + gitSuffix
	})
	// Swap any remaining bare host references (no path component).
	return r.bare.Replace(out)
}

func splitGit(path string) (core, suffix string) {
	if strings.HasSuffix(path, ".git") {
		return strings.TrimSuffix(path, ".git"), ".git"
	}
	return path, ""
}

// Run rewrites the tree rooted at root and returns the list of changed files.
func Run(root string, opt Options) ([]Change, error) {
	oldHost := hostOf(opt.OldURL)
	newHost := hostOf(opt.NewURL)
	if oldHost == "" || newHost == "" {
		return nil, nil
	}
	// Nothing to do only if host is unchanged AND there is neither a path
	// remap nor a prefix that could still alter references.
	if oldHost == newHost && len(opt.Paths) == 0 && trimPath(opt.Prefix) == "" {
		return nil, nil
	}
	maxSize := opt.MaxFileSize
	if maxSize == 0 {
		maxSize = defaultMaxFileSize
	}
	rw := newRewriter(oldHost, newHost, opt.Prefix, opt.Paths)

	targets := map[string]struct{}{}

	// All files directly in the root directory.
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		targets[filepath.Join(root, e.Name())] = struct{}{}
	}

	// Every composer.json at any depth (skipping .git, vendor, node_modules).
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "vendor", "node_modules":
				if path != root {
					return fs.SkipDir
				}
			}
			return nil
		}
		if d.Name() == "composer.json" {
			targets[path] = struct{}{}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	var changes []Change
	for path := range targets {
		c, err := rewriteFile(path, root, rw, oldHost, maxSize)
		if err != nil {
			return nil, err
		}
		if c != nil {
			changes = append(changes, *c)
		}
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].Path < changes[j].Path })
	return changes, nil
}

func rewriteFile(path, root string, rw *rewriter, oldHost string, maxSize int64) (*Change, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxSize {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if bytes.IndexByte(data, 0) != -1 {
		return nil, nil // binary
	}
	content := string(data)
	if !strings.Contains(content, oldHost) {
		return nil, nil
	}
	updated := rw.apply(content)
	if updated == content {
		return nil, nil
	}
	if err := os.WriteFile(path, []byte(updated), info.Mode().Perm()); err != nil {
		return nil, err
	}
	rel, _ := filepath.Rel(root, path)
	return &Change{Path: rel, Occurrences: strings.Count(content, oldHost)}, nil
}
