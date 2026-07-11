// Package containerreg copies container images between GitLab registries using
// the skopeo binary (registry-to-registry, no Docker daemon required).
package containerreg

import (
	"fmt"
	"os/exec"
	"strings"
)

// Creds are registry credentials (GitLab: username + a token with registry
// access, e.g. a PAT with the api or read_registry/write_registry scope).
type Creds struct {
	User  string
	Token string
}

// Available reports whether the skopeo binary is on PATH.
func Available() bool {
	_, err := exec.LookPath("skopeo")
	return err == nil
}

// TargetImage maps a source image location to the target one by swapping the
// source registry/project prefix for the target prefix, preserving the image
// name suffix within the project.
func TargetImage(srcLocation, srcPrefix, dstPrefix string) string {
	suffix := strings.TrimPrefix(srcLocation, strings.TrimRight(srcPrefix, "/"))
	return strings.TrimRight(dstPrefix, "/") + suffix
}

// Copy copies one image:tag from source to target via `skopeo copy --all`
// (all architectures / the full manifest list).
func Copy(srcImage, dstImage, tag string, src, dst Creds) error {
	args := []string{
		"copy", "--all",
		"--src-creds", src.User + ":" + src.Token,
		"--dest-creds", dst.User + ":" + dst.Token,
		"docker://" + srcImage + ":" + tag,
		"docker://" + dstImage + ":" + tag,
	}
	cmd := exec.Command("skopeo", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("skopeo copy %s:%s -> %s:%s: %v: %s",
			srcImage, tag, dstImage, tag, err, strings.TrimSpace(string(out)))
	}
	return nil
}
