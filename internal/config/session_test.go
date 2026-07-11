package config

import (
	"testing"
	"time"
)

func TestSessionRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	s := &Session{
		Name:        "src→dst",
		Source:      Endpoint{URL: "https://old", Token: "t1", Transport: TransportAuto},
		Target:      Endpoint{URL: "https://new", Token: "${TGT_TOKEN}", Transport: TransportSSH},
		Selected:    []int64{5, 9},
		Assignments: map[int64]string{5: "group/a", 9: "group/b"},
		Options:     Options{Issues: true, URLRewrite: true},
	}
	if err := Save(s, time.Now()); err != nil {
		t.Fatal(err)
	}

	names, err := List()
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 {
		t.Fatalf("expected 1 session, got %v", names)
	}

	got, err := Load(names[0])
	if err != nil {
		t.Fatal(err)
	}
	if got.Source.URL != "https://old" || got.Target.Transport != TransportSSH {
		t.Errorf("endpoint mismatch: %+v", got)
	}
	if len(got.Selected) != 2 || got.Assignments[9] != "group/b" {
		t.Errorf("selection/mapping mismatch: %+v", got)
	}
	if got.UpdatedAt == "" {
		t.Error("UpdatedAt not stamped")
	}

	if err := Remove(names[0]); err != nil {
		t.Fatal(err)
	}
	names, _ = List()
	if len(names) != 0 {
		t.Errorf("expected 0 sessions after remove, got %v", names)
	}
}

func TestSessionPathMapRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	s := &Session{
		Name:    "pm",
		Source:  Endpoint{URL: "https://old"},
		Target:  Endpoint{URL: "https://new"},
		PathMap: map[string]string{"old/group/lib": "new/group/lib"},
	}
	if err := Save(s, time.Now()); err != nil {
		t.Fatal(err)
	}
	got, err := Load("pm")
	if err != nil {
		t.Fatal(err)
	}
	if got.PathMap["old/group/lib"] != "new/group/lib" {
		t.Errorf("session path map round-trip failed: %v", got.PathMap)
	}
}

func TestClearState(t *testing.T) {
	s := &Session{
		Name:        "x",
		Source:      Endpoint{URL: "https://old", Token: "keepme"},
		Target:      Endpoint{URL: "https://new", Token: "keeptoo"},
		Selected:    []int64{1, 2},
		Assignments: map[int64]string{1: "g/a"},
		Force:       []int64{1},
		PathMap:     map[string]string{"a": "b"},
		Options:     Options{Issues: true},
	}
	s.ClearState()
	if len(s.Selected) != 0 || len(s.Assignments) != 0 || len(s.Force) != 0 || len(s.PathMap) != 0 {
		t.Errorf("state not cleared: %+v", s)
	}
	// Config + tokens + options must survive.
	if s.Source.Token != "keepme" || s.Target.Token != "keeptoo" || s.Source.URL != "https://old" || !s.Options.Issues {
		t.Errorf("config/tokens/options should be kept: %+v", s)
	}
}

func TestResolveToken(t *testing.T) {
	t.Setenv("MY_TOKEN", "secret-value")
	if got := ResolveToken("${MY_TOKEN}"); got != "secret-value" {
		t.Errorf("env ref not resolved: %q", got)
	}
	if got := ResolveToken("glpat-literal"); got != "glpat-literal" {
		t.Errorf("literal token altered: %q", got)
	}
	if got := ResolveToken("${MISSING_TOKEN}"); got != "" {
		t.Errorf("missing env should resolve empty, got %q", got)
	}
}
