// Package gittransport performs the version-independent repository copy using
// the system git binary: `git clone --mirror` followed by `git push --mirror`.
//
// It selects between SSH and HTTPS-with-token transports and implements the
// existing-target guard that refuses to overwrite a target repo which already
// holds newer or divergent history.
package gittransport

import (
	"bytes"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/bresam/gitlab-copy-tool/internal/config"
)

// Repo identifies one side of the copy.
type Repo struct {
	SSHURL    string
	HTTPURL   string
	Token     string
	Transport string // auto|ssh|https
}

// Spec is the input to Mirror.
type Spec struct {
	Source Repo
	Target Repo
	// WorkDir is a directory the mirror clone is created in (a subdir is used).
	WorkDir string
	// Force disables the existing-target guard (always overwrite).
	Force bool
}

// Result reports the outcome of a mirror operation.
type Result struct {
	Pushed  bool
	Skipped bool
	Reason  string
	// UpToDate is true when the target already has exactly the same branches and
	// tags as the source, so nothing was cloned or pushed.
	UpToDate bool
	// Forced is true when the target held newer/divergent history but was
	// overwritten anyway because Force was set. Reason then carries the detail.
	Forced bool
	// CloneDir is the bare mirror clone directory (kept for follow-up steps
	// like the URL rewrite). Caller is responsible for removing it.
	CloneDir string
	// TargetURL is the resolved target URL (with credentials) that worked;
	// reusable by follow-up steps such as the URL-rewrite commit.
	TargetURL string
}

// Logf is an optional progress sink.
type Logf func(format string, args ...any)

const guardPrefix = "refs/gct-guard/"

// Mirror clones the source and pushes it to the target, honouring the guard.
func Mirror(spec Spec, logf Logf) (Result, error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	var res Result

	// 1. Resolve the target and compare refs first — if the target already has
	//    exactly the source's branches and tags, skip the clone/push entirely.
	targetURL, targetRefs, err := resolveTarget(spec.Target, logf)
	if err != nil {
		return res, err
	}
	res.TargetURL = targetURL
	if len(targetRefs) > 0 {
		if srcRefs, err := lsRemoteAny(spec.Source); err == nil && refsEqual(srcRefs, targetRefs) {
			res.UpToDate = true
			logf("target already up to date (%d refs) — skipping clone", len(targetRefs))
			return res, nil
		}
	}

	dir := filepath.Join(spec.WorkDir, "mirror.git")
	_ = os.RemoveAll(dir)

	// 2. Clone --mirror from the source, trying candidate transports.
	srcURLs := candidates(spec.Source)
	var cloneErr error
	cloned := false
	for _, u := range srcURLs {
		logf("clone --mirror via %s", redact(u))
		if _, err := runGit("", "clone", "--mirror", u, dir); err != nil {
			cloneErr = err
			_ = os.RemoveAll(dir)
			continue
		}
		cloned = true
		break
	}
	if !cloned {
		return res, fmt.Errorf("clone failed: %w", cloneErr)
	}
	res.CloneDir = dir

	// 3. Existing-target guard. Always evaluated when the target has refs so we
	//    can report the reason; Force decides whether to overwrite anyway.
	if len(targetRefs) > 0 {
		reason, err := guard(dir, targetURL, targetRefs, logf)
		if err != nil {
			return res, err
		}
		if reason != "" {
			if !spec.Force {
				res.Skipped = true
				res.Reason = reason
				return res, nil
			}
			// Force: overwrite, but surface what was overwritten.
			res.Forced = true
			res.Reason = reason
			logf("force: overwriting despite: %s", reason)
		}
	}

	// 4. Push branches and tags only (force + prune), NOT GitLab-internal hidden
	//    refs. A `git push --mirror` would also try to push refs/merge-requests/*,
	//    refs/pipelines/*, refs/keep-around/* etc. from the source mirror, which
	//    GitLab rejects server-side ("pre-receive hook declined").
	logf("push branches+tags via %s", redact(targetURL))
	if _, err := runGit(dir, "push", "--force", "--prune", targetURL,
		"refs/heads/*:refs/heads/*", "refs/tags/*:refs/tags/*"); err != nil {
		return res, fmt.Errorf("push: %w", err)
	}
	res.Pushed = true
	return res, nil
}

// resolveTarget returns the first target URL that answers ls-remote and its
// current refs (logical name -> sha).
func resolveTarget(r Repo, logf Logf) (string, map[string]string, error) {
	var lastErr error
	for _, u := range candidates(r) {
		refs, err := lsRemote(u)
		if err != nil {
			lastErr = err
			continue
		}
		return u, refs, nil
	}
	return "", nil, fmt.Errorf("cannot reach target: %w", lastErr)
}

// lsRemoteAny returns the heads+tags of a repo, trying its candidate URLs.
func lsRemoteAny(r Repo) (map[string]string, error) {
	var lastErr error
	for _, u := range candidates(r) {
		refs, err := lsRemote(u)
		if err != nil {
			lastErr = err
			continue
		}
		return refs, nil
	}
	return nil, lastErr
}

// refsEqual reports whether two ref maps (logical name -> sha) are identical.
func refsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// guard fetches the target refs into the local mirror and compares them with
// the source. It returns a non-empty human-readable reason when the target
// holds newer or divergent history (meaning: do NOT overwrite).
func guard(dir, targetURL string, targetRefs map[string]string, logf Logf) (string, error) {
	logf("existing target has %d refs, checking for newer content", len(targetRefs))
	// Fetch target objects/refs under the guard namespace.
	_, err := runGit(dir, "fetch", "--quiet", targetURL,
		"+refs/heads/*:"+guardPrefix+"heads/*",
		"+refs/tags/*:"+guardPrefix+"tags/*")
	if err != nil {
		return "", fmt.Errorf("guard fetch: %w", err)
	}

	sourceRefs, err := localRefs(dir)
	if err != nil {
		return "", err
	}

	for name, tgtSha := range targetRefs {
		srcSha, ok := sourceRefs[name]
		if !ok {
			return fmt.Sprintf("target ref %q exists only on the target", name), nil
		}
		if srcSha == tgtSha {
			continue
		}
		// Is the target tip an ancestor of the source tip? If yes the target
		// is merely behind and safe to overwrite; if no it has newer/divergent
		// commits.
		if !isAncestor(dir, tgtSha, srcSha) {
			return fmt.Sprintf("target ref %q has newer or divergent commits", name), nil
		}
	}
	return "", nil
}

// candidates returns the ordered list of URLs to try for a repo.
func candidates(r Repo) []string {
	switch r.Transport {
	case config.TransportSSH:
		return []string{r.SSHURL}
	case config.TransportHTTPS:
		return []string{httpsWithToken(r.HTTPURL, r.Token)}
	default: // auto
		var out []string
		if r.SSHURL != "" {
			out = append(out, r.SSHURL)
		}
		if r.HTTPURL != "" {
			out = append(out, httpsWithToken(r.HTTPURL, r.Token))
		}
		return out
	}
}

// httpsWithToken injects oauth2:<token> credentials into an https clone URL.
func httpsWithToken(httpURL, token string) string {
	if httpURL == "" || token == "" {
		return httpURL
	}
	u, err := url.Parse(httpURL)
	if err != nil {
		return httpURL
	}
	u.User = url.UserPassword("oauth2", token)
	return u.String()
}

// redact hides credentials in a URL for logging.
func redact(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	u.User = url.User("***")
	return u.String()
}

// --- git helpers ---------------------------------------------------------

func gitEnv() []string {
	return append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_SSH_COMMAND=ssh -o StrictHostKeyChecking=accept-new -o BatchMode=yes",
	)
}

func runGit(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = gitEnv()
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("git %s: %v: %s",
			strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

// lsRemote returns branches and tags of a remote as logical-name -> sha.
func lsRemote(u string) (map[string]string, error) {
	out, err := runGit("", "ls-remote", "--heads", "--tags", u)
	if err != nil {
		return nil, err
	}
	refs := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		sha, ref := fields[0], fields[1]
		ref = strings.TrimSuffix(ref, "^{}") // peeled tag
		refs[logicalName(ref)] = sha
	}
	return refs, nil
}

// localRefs returns the mirror's heads and tags as logical-name -> sha.
func localRefs(dir string) (map[string]string, error) {
	out, err := runGit(dir, "show-ref")
	if err != nil {
		// A repo with no refs yields exit code 1 and empty output.
		if strings.TrimSpace(out) == "" {
			return map[string]string{}, nil
		}
		return nil, err
	}
	refs := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		sha, ref := fields[0], fields[1]
		if strings.HasPrefix(ref, guardPrefix) {
			continue
		}
		refs[logicalName(ref)] = sha
	}
	return refs, nil
}

// logicalName strips known ref prefixes to a comparable name like
// "heads/main" or "tags/v1".
func logicalName(ref string) string {
	for _, p := range []string{"refs/heads/", "refs/tags/", guardPrefix + "heads/", guardPrefix + "tags/"} {
		if strings.HasPrefix(ref, p) {
			kind := "heads/"
			if strings.Contains(p, "tags/") {
				kind = "tags/"
			}
			return kind + strings.TrimPrefix(ref, p)
		}
	}
	return strings.TrimPrefix(ref, "refs/")
}

func isAncestor(dir, ancestor, descendant string) bool {
	cmd := exec.Command("git", "merge-base", "--is-ancestor", ancestor, descendant)
	cmd.Dir = dir
	cmd.Env = gitEnv()
	return cmd.Run() == nil
}
