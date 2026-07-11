package rewrite

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestRun(t *testing.T) {
	root := t.TempDir()
	old := "https://gitlab.internal.example.com"
	neu := "https://gitlab.com"

	// Root composer.json with old URL (twice) and a trailing-slash variant.
	writeFile(t, filepath.Join(root, "composer.json"),
		`{"repositories":[{"url":"`+old+`/group/lib.git"},{"url":"`+old+`"}]}`)
	// Nested composer.json (should be rewritten too).
	writeFile(t, filepath.Join(root, "packages", "a", "composer.json"),
		`{"url":"`+old+`/x.git"}`)
	// Root file that is not composer.json (should be rewritten).
	writeFile(t, filepath.Join(root, "README.md"), "See "+old+"/docs")
	// Nested non-composer file (should NOT be rewritten).
	nested := filepath.Join(root, "src", "config.php")
	writeFile(t, nested, "url="+old)
	// File in vendor (should be skipped).
	writeFile(t, filepath.Join(root, "vendor", "composer.json"), old)

	changes, err := Run(root, Options{OldURL: old, NewURL: neu})
	if err != nil {
		t.Fatal(err)
	}

	if got := read(t, filepath.Join(root, "composer.json")); got !=
		`{"repositories":[{"url":"`+neu+`/group/lib.git"},{"url":"`+neu+`"}]}` {
		t.Errorf("root composer.json not rewritten correctly: %s", got)
	}
	if got := read(t, filepath.Join(root, "packages", "a", "composer.json")); got != `{"url":"`+neu+`/x.git"}` {
		t.Errorf("nested composer.json not rewritten: %s", got)
	}
	if got := read(t, filepath.Join(root, "README.md")); got != "See "+neu+"/docs" {
		t.Errorf("root README not rewritten: %s", got)
	}
	if got := read(t, nested); got != "url="+old {
		t.Errorf("nested non-composer file should be untouched: %s", got)
	}
	if got := read(t, filepath.Join(root, "vendor", "composer.json")); got != old {
		t.Errorf("vendor file should be skipped: %s", got)
	}

	if len(changes) != 3 {
		t.Errorf("expected 3 changed files, got %d: %+v", len(changes), changes)
	}
}

func TestRunSSHVariants(t *testing.T) {
	root := t.TempDir()
	old := "https://gitlab.internal.example.com"
	neu := "https://gitlab.com"

	// scp-like SSH remote, ssh:// remote and an https remote in one root file.
	writeFile(t, filepath.Join(root, "remotes.txt"),
		"git@gitlab.internal.example.com:grp/x.git\n"+
			"ssh://git@gitlab.internal.example.com/grp/y.git\n"+
			"https://gitlab.internal.example.com/grp/z.git\n")

	if _, err := Run(root, Options{OldURL: old, NewURL: neu}); err != nil {
		t.Fatal(err)
	}
	got := read(t, filepath.Join(root, "remotes.txt"))
	want := "git@gitlab.com:grp/x.git\n" +
		"ssh://git@gitlab.com/grp/y.git\n" +
		"https://gitlab.com/grp/z.git\n"
	if got != want {
		t.Errorf("ssh variants not rewritten:\n got: %q\nwant: %q", got, want)
	}
}

func TestRunPathMapping(t *testing.T) {
	root := t.TempDir()
	old := "https://gitlab.internal.example.com"
	neu := "https://gitlab.com"

	writeFile(t, filepath.Join(root, "composer.json"),
		"https://gitlab.internal.example.com/grp/a.git\n"+
			"git@gitlab.internal.example.com:grp/a.git\n"+
			"https://gitlab.internal.example.com/other/dep.git\n")

	_, err := Run(root, Options{
		OldURL: old, NewURL: neu,
		Paths: []PathMapping{{OldPath: "grp/a", NewPath: "newgrp/a"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := read(t, filepath.Join(root, "composer.json"))
	want := "https://gitlab.com/newgrp/a.git\n" + // host+path remapped
		"git@gitlab.com:newgrp/a.git\n" + // scp-like host+path remapped
		"https://gitlab.com/other/dep.git\n" // not migrated: host only
	if got != want {
		t.Errorf("path mapping wrong:\n got: %q\nwant: %q", got, want)
	}
}

func TestRunPathMappingSameHost(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "composer.json"), "https://git.example.com/old/lib.git")
	_, err := Run(root, Options{
		OldURL: "https://git.example.com", NewURL: "https://git.example.com",
		Paths: []PathMapping{{OldPath: "old/lib", NewPath: "new/lib"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(root, "composer.json")); got != "https://git.example.com/new/lib.git" {
		t.Errorf("same-host path remap failed: %q", got)
	}
}

func TestRunAccountPrefix(t *testing.T) {
	root := t.TempDir()
	old := "https://gitlab.internal.example.com"
	neu := "https://gitlab.com"

	writeFile(t, filepath.Join(root, "composer.json"),
		"https://gitlab.internal.example.com/tools/dep.git\n"+ // not migrated -> prefix
			"git@gitlab.internal.example.com:tools/dep.git\n"+ // scp, keep form + prefix
			"https://gitlab.internal.example.com/mine/lib.git\n") // migrated -> exact map

	_, err := Run(root, Options{
		OldURL: old, NewURL: neu,
		Prefix: "example-org",
		Paths:  []PathMapping{{OldPath: "mine/lib", NewPath: "example-org/mine/lib"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := read(t, filepath.Join(root, "composer.json"))
	want := "https://gitlab.com/example-org/tools/dep.git\n" + // host + prefix
		"git@gitlab.com:example-org/tools/dep.git\n" + // scp form kept, prefix added
		"https://gitlab.com/example-org/mine/lib.git\n" // exact path map
	if got != want {
		t.Errorf("account prefix rewrite wrong:\n got: %q\nwant: %q", got, want)
	}
}

func TestRunPrefixNotDoubled(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "composer.json"),
		"https://gitlab.internal.example.com/example-org/already.git")
	_, err := Run(root, Options{
		OldURL: "https://gitlab.internal.example.com", NewURL: "https://gitlab.com",
		Prefix: "example-org",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(root, "composer.json")); got != "https://gitlab.com/example-org/already.git" {
		t.Errorf("prefix should not be doubled: %q", got)
	}
}

// Divergent move: tools/test was migrated to a different namespace
// (example-org/abweichender-space/tools/test). A later repo that depends on
// it must be rewritten to that exact new path, not the static prefix path.
func TestRunDivergentPathMapping(t *testing.T) {
	root := t.TempDir()
	old := "https://gitlab.internal.example.com"
	neu := "https://gitlab.com"

	writeFile(t, filepath.Join(root, "composer.json"),
		"https://gitlab.internal.example.com/tools/test.git\n"+ // moved dep (in map)
			"git@gitlab.internal.example.com:tools/test.git\n"+ // same, scp form
			"https://gitlab.internal.example.com/tools/other.git\n") // not migrated -> prefix

	_, err := Run(root, Options{
		OldURL: old, NewURL: neu,
		Prefix: "example-org", // account of the repo being migrated now
		Paths: []PathMapping{{
			OldPath: "tools/test",
			NewPath: "example-org/abweichender-space/tools/test",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := read(t, filepath.Join(root, "composer.json"))
	want := "https://gitlab.com/example-org/abweichender-space/tools/test.git\n" +
		"git@gitlab.com:example-org/abweichender-space/tools/test.git\n" +
		"https://gitlab.com/example-org/tools/other.git\n" // prefix fallback
	if got != want {
		t.Errorf("divergent path mapping wrong:\n got: %q\nwant: %q", got, want)
	}
}

func TestRunNoOccurrences(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "composer.json"), `{"name":"x/y"}`)
	changes, err := Run(root, Options{OldURL: "https://a", NewURL: "https://b"})
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 0 {
		t.Errorf("expected no changes, got %+v", changes)
	}
}

func TestRunSameURLNoop(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "composer.json"), "https://same")
	changes, err := Run(root, Options{OldURL: "https://same", NewURL: "https://same"})
	if err != nil {
		t.Fatal(err)
	}
	if changes != nil {
		t.Errorf("expected nil changes for identical URLs")
	}
}
