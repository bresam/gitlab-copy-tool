package gittransport

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bresam/gitlab-copy-tool/internal/config"
)

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
		"GIT_TERMINAL_PROMPT=0",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func commitFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, dir, "add", "-A")
	gitT(t, dir, "commit", "-m", "commit "+name)
}

func mainSha(t *testing.T, repo string) string {
	refs, err := lsRemote(repo)
	if err != nil {
		t.Fatal(err)
	}
	return refs["heads/main"]
}

func TestMirrorEndToEnd(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "src")
	tgt := filepath.Join(base, "target.git")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}

	// Build a source repo with two branches and a tag.
	gitT(t, src, "init", "-b", "main")
	commitFile(t, src, "a.txt", "one")
	gitT(t, src, "branch", "feature")
	gitT(t, src, "tag", "-m", "release 1", "v1")

	gitT(t, base, "init", "--bare", "-b", "main", "target.git")

	spec := func(force bool) Spec {
		return Spec{
			Source:  Repo{HTTPURL: src, Transport: config.TransportHTTPS},
			Target:  Repo{HTTPURL: tgt, Transport: config.TransportHTTPS},
			WorkDir: t.TempDir(),
			Force:   force,
		}
	}

	// 1. Push into an empty target.
	res, err := Mirror(spec(false), nil)
	if err != nil {
		t.Fatalf("initial mirror: %v", err)
	}
	if !res.Pushed || res.Skipped {
		t.Fatalf("expected push, got %+v", res)
	}
	refs, _ := lsRemote(tgt)
	for _, want := range []string{"heads/main", "heads/feature", "tags/v1"} {
		if _, ok := refs[want]; !ok {
			t.Errorf("target missing ref %s (have %v)", want, refs)
		}
	}

	// 1b. Re-run with no changes -> up to date, nothing pushed/cloned.
	res, err = Mirror(spec(false), nil)
	if err != nil {
		t.Fatalf("up-to-date mirror: %v", err)
	}
	if !res.UpToDate || res.Pushed {
		t.Fatalf("expected UpToDate with no push, got %+v", res)
	}

	// 2. Target behind (source advanced) -> overwrite.
	commitFile(t, src, "b.txt", "two")
	srcSha := mainSha(t, src)
	res, err = Mirror(spec(false), nil)
	if err != nil {
		t.Fatalf("second mirror: %v", err)
	}
	if !res.Pushed || res.Skipped {
		t.Fatalf("expected overwrite of behind target, got %+v", res)
	}
	if got := mainSha(t, tgt); got != srcSha {
		t.Errorf("target main not updated: got %s want %s", got, srcSha)
	}

	// 3. Target has a divergent/newer commit -> skip with reason.
	clone := filepath.Join(base, "clone")
	gitT(t, base, "clone", tgt, "clone")
	commitFile(t, clone, "target-only.txt", "extra")
	gitT(t, clone, "push", "origin", "main")

	res, err = Mirror(spec(false), nil)
	if err != nil {
		t.Fatalf("third mirror: %v", err)
	}
	if !res.Skipped {
		t.Fatalf("expected skip on divergent target, got %+v", res)
	}
	if !strings.Contains(res.Reason, "newer or divergent") {
		t.Errorf("unexpected skip reason: %q", res.Reason)
	}

	// 4. Force overrides the guard but still reports what it overwrote.
	res, err = Mirror(spec(true), nil)
	if err != nil {
		t.Fatalf("forced mirror: %v", err)
	}
	if !res.Pushed || res.Skipped {
		t.Fatalf("expected forced push, got %+v", res)
	}
	if !res.Forced || res.Reason == "" {
		t.Errorf("expected Forced=true with a reason, got %+v", res)
	}
	if got := mainSha(t, tgt); got != srcSha {
		t.Errorf("forced overwrite did not reset target main: got %s want %s", got, srcSha)
	}
}

func TestGuardTargetOnlyRef(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "src")
	tgt := filepath.Join(base, "target.git")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	gitT(t, src, "init", "-b", "main")
	commitFile(t, src, "a.txt", "one")
	gitT(t, base, "init", "--bare", "-b", "main", "target.git")

	spec := Spec{
		Source:  Repo{HTTPURL: src, Transport: config.TransportHTTPS},
		Target:  Repo{HTTPURL: tgt, Transport: config.TransportHTTPS},
		WorkDir: t.TempDir(),
	}
	if _, err := Mirror(spec, nil); err != nil {
		t.Fatal(err)
	}

	// Add a target-only branch.
	clone := filepath.Join(base, "clone")
	gitT(t, base, "clone", tgt, "clone")
	gitT(t, clone, "checkout", "-b", "only-on-target")
	commitFile(t, clone, "x.txt", "x")
	gitT(t, clone, "push", "origin", "only-on-target")

	spec.WorkDir = t.TempDir()
	res, err := Mirror(spec, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Skipped || !strings.Contains(res.Reason, "only on the target") {
		t.Errorf("expected target-only skip, got %+v", res)
	}
}
