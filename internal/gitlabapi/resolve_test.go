package gitlabapi

import (
	"reflect"
	"sort"
	"testing"

	"github.com/bresam/gitlab-copy-tool/internal/config"
)

// tools/                (group id 1)
//
//	deployment          (project id 10, in "tools")
//	ci/                 (group id 2, "tools/ci")
//	  runner            (project id 11, in "tools/ci")
func sampleTree() []*Node {
	return []*Node{{
		ID: 1, Kind: "group", Name: "tools", Path: "tools", FullPath: "tools",
		Children: []*Node{
			{ID: 10, Kind: "project", Name: "deployment", Path: "deployment", FullPath: "tools/deployment", NamespacePath: "tools"},
			{ID: 2, Kind: "group", Name: "ci", Path: "ci", FullPath: "tools/ci", Children: []*Node{
				{ID: 11, Kind: "project", Name: "runner", Path: "runner", FullPath: "tools/ci/runner", NamespacePath: "tools/ci"},
			}},
		},
	}}
}

func TestResolveTargetsCascade(t *testing.T) {
	roots := sampleTree()
	assign := map[int64]string{1: "example-org"} // group-level assignment
	selected := map[int64]bool{10: true, 11: true}

	targets, unmapped := ResolveTargets(roots, assign, selected)
	if len(unmapped) != 0 {
		t.Fatalf("expected all mapped, unmapped=%v", unmapped)
	}
	if targets[10] != "example-org/tools" {
		t.Errorf("deployment target = %q, want example-org/tools", targets[10])
	}
	if targets[11] != "example-org/tools/ci" {
		t.Errorf("runner target = %q, want example-org/tools/ci (substructure preserved)", targets[11])
	}
}

func TestResolveTargetsOverride(t *testing.T) {
	roots := sampleTree()
	assign := map[int64]string{1: "example-org", 11: "acme/special"} // per-repo override
	selected := map[int64]bool{10: true, 11: true}

	targets, _ := ResolveTargets(roots, assign, selected)
	if targets[10] != "example-org/tools" {
		t.Errorf("deployment = %q", targets[10])
	}
	if targets[11] != "acme/special" {
		t.Errorf("runner override = %q, want acme/special", targets[11])
	}
}

func TestResolveTargetsSubgroupOverride(t *testing.T) {
	roots := sampleTree()
	// Override on the ci subgroup; runner should nest under it.
	assign := map[int64]string{1: "example-org", 2: "acme"}
	selected := map[int64]bool{11: true}

	targets, _ := ResolveTargets(roots, assign, selected)
	if targets[11] != "acme/ci" {
		t.Errorf("runner under subgroup override = %q, want acme/ci", targets[11])
	}
}

func TestTargetVisibility(t *testing.T) {
	cases := map[string]string{
		"internal": "private", // no SaaS equivalent, must not be public
		"private":  "private",
		"public":   "public",
		"":         "",
	}
	for in, want := range cases {
		if got := TargetVisibility(in); got != want {
			t.Errorf("TargetVisibility(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBuildGroupHintsMapsInternal(t *testing.T) {
	roots := []*Node{{ID: 1, Kind: "group", Name: "Team", Path: "team", FullPath: "team", Visibility: "internal"}}
	if got := BuildGroupHints(roots)["team"].Visibility; got != "private" {
		t.Errorf("group internal visibility should map to private, got %q", got)
	}
}

func TestBuildGroupHints(t *testing.T) {
	roots := []*Node{{
		ID: 1, Kind: "group", Name: "Public", Path: "pub", FullPath: "pub", Visibility: "public",
		Children: []*Node{
			{ID: 2, Kind: "group", Name: "Tools", Path: "tools", FullPath: "pub/tools", Visibility: "internal"},
			{ID: 10, Kind: "project", Name: "app", Path: "app", FullPath: "pub/app", Visibility: "public"},
		},
	}}
	hints := BuildGroupHints(roots)
	if got := hints["pub"]; got.Name != "Public" || got.Visibility != "public" {
		t.Errorf(`hints["pub"] = %+v, want {Public public}`, got)
	}
	if got := hints["tools"]; got.Name != "Tools" || got.Visibility != "private" {
		t.Errorf(`hints["tools"] = %+v, want {Tools private} (internal maps to private)`, got)
	}
	if _, ok := hints["app"]; ok {
		t.Errorf("projects should not be in group hints")
	}
}

func TestResolveOptionsCascadeAndOverride(t *testing.T) {
	roots := sampleTree()
	baseline := config.Options{} // everything off
	// Group "tools" enables Releases; project "runner" overrides it back off.
	overrides := map[int64]map[int]bool{
		1:  {config.OptReleases: true},
		11: {config.OptReleases: false},
	}
	got := ResolveOptions(roots, overrides, baseline, map[int64]bool{10: true, 11: true})

	if !got[10].Releases {
		t.Errorf("deployment should inherit Releases=true from group tools")
	}
	if got[11].Releases {
		t.Errorf("runner should override Releases back to false")
	}
	if got[10].Issues || got[10].ContainerRegistry {
		t.Errorf("unset options should stay at baseline (false): %+v", got[10])
	}
}

func TestResolveTargetsUnmapped(t *testing.T) {
	roots := sampleTree()
	targets, unmapped := ResolveTargets(roots, map[int64]string{}, map[int64]bool{10: true, 11: true})
	if len(targets) != 0 {
		t.Errorf("expected no targets, got %v", targets)
	}
	sort.Slice(unmapped, func(i, j int) bool { return unmapped[i] < unmapped[j] })
	if !reflect.DeepEqual(unmapped, []int64{10, 11}) {
		t.Errorf("unmapped = %v, want [10 11]", unmapped)
	}
}
