package containerreg

import "testing"

func TestTargetImage(t *testing.T) {
	cases := []struct {
		name, srcLocation, srcPrefix, dstPrefix, want string
	}{
		{
			name:        "image under project",
			srcLocation: "registry.src.example.com/grp/proj/api",
			srcPrefix:   "registry.src.example.com/grp/proj",
			dstPrefix:   "registry.gitlab.com/example-org/grp/proj",
			want:        "registry.gitlab.com/example-org/grp/proj/api",
		},
		{
			name:        "image at project root (no sub-name)",
			srcLocation: "registry.src.example.com/grp/proj",
			srcPrefix:   "registry.src.example.com/grp/proj",
			dstPrefix:   "registry.gitlab.com/example-org/grp/proj",
			want:        "registry.gitlab.com/example-org/grp/proj",
		},
		{
			name:        "nested image name",
			srcLocation: "registry.src.example.com/grp/proj/svc/worker",
			srcPrefix:   "registry.src.example.com/grp/proj",
			dstPrefix:   "registry.gitlab.com/example-org/proj",
			want:        "registry.gitlab.com/example-org/proj/svc/worker",
		},
	}
	for _, c := range cases {
		if got := TargetImage(c.srcLocation, c.srcPrefix, c.dstPrefix); got != c.want {
			t.Errorf("%s: TargetImage()=%q, want %q", c.name, got, c.want)
		}
	}
}

func TestRegistryHost(t *testing.T) {
	if got := registryHost("registry.gitlab.com/grp/proj/img"); got != "registry.gitlab.com" {
		t.Errorf("registryHost = %q", got)
	}
}
