// Package containerreg copies container images between GitLab registries.
//
// It uses the pure-Go go-containerregistry (crane) library and copies images
// registry-to-registry in-process — no external binary (skopeo/docker) and no
// Docker daemon are required, so it works out of the box with the distributed
// binary.
package containerreg

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/crane"
)

// copyTimeout bounds a single image:tag copy so a broken or hanging tag on the
// source cannot block the whole run.
const copyTimeout = 10 * time.Minute

// Creds are registry credentials (GitLab: username + a token with registry
// access, e.g. a PAT with the api or read_registry/write_registry scope).
type Creds struct {
	User  string
	Token string
}

// TargetImage maps a source image location to the target one by swapping the
// source registry/project prefix for the target prefix, preserving the image
// name suffix within the project.
func TargetImage(srcLocation, srcPrefix, dstPrefix string) string {
	suffix := strings.TrimPrefix(srcLocation, strings.TrimRight(srcPrefix, "/"))
	return strings.TrimRight(dstPrefix, "/") + suffix
}

// hostKeychain resolves registry credentials per registry host.
type hostKeychain map[string]authn.Authenticator

func (k hostKeychain) Resolve(r authn.Resource) (authn.Authenticator, error) {
	if a, ok := k[r.RegistryStr()]; ok {
		return a, nil
	}
	return authn.Anonymous, nil
}

// registryHost returns the registry hostname of an image reference
// (everything before the first "/").
func registryHost(image string) string {
	if i := strings.IndexByte(image, '/'); i >= 0 {
		return image[:i]
	}
	return image
}

// Copy copies one image:tag from source to target, including all platforms of a
// multi-arch manifest list. Source and target may live on different registries
// with different credentials. It refuses to write to the source, checks the
// source manifest first (so broken/empty tags are skipped fast), and bounds the
// whole operation with a timeout so a hanging tag can't block the run.
func Copy(srcImage, dstImage, tag string, src, dst Creds) error {
	srcRef := srcImage + ":" + tag
	dstRef := dstImage + ":" + tag
	if srcImage == dstImage {
		return fmt.Errorf("refusing to copy: target image equals source (%s)", srcRef)
	}

	kc := hostKeychain{
		registryHost(srcImage): &authn.Basic{Username: src.User, Password: src.Token},
		registryHost(dstImage): &authn.Basic{Username: dst.User, Password: dst.Token},
	}
	ctx, cancel := context.WithTimeout(context.Background(), copyTimeout)
	defer cancel()
	opts := []crane.Option{crane.WithAuthFromKeychain(kc), crane.WithContext(ctx)}

	// Fast pre-check: a broken/empty source tag (no readable manifest) is
	// skipped with a clear error instead of hanging the copy.
	if _, err := crane.Head(srcRef, opts...); err != nil {
		return fmt.Errorf("source tag %s unreadable, skipped: %w", srcRef, err)
	}
	if err := crane.Copy(srcRef, dstRef, opts...); err != nil {
		return fmt.Errorf("copy %s -> %s: %w", srcRef, dstRef, err)
	}
	return nil
}
