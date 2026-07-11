// Package containerreg copies container images between GitLab registries.
//
// It uses the pure-Go go-containerregistry (crane) library and copies images
// registry-to-registry in-process — no external binary (skopeo/docker) and no
// Docker daemon are required, so it works out of the box with the distributed
// binary.
package containerreg

import (
	"fmt"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/crane"
)

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
// with different credentials.
func Copy(srcImage, dstImage, tag string, src, dst Creds) error {
	kc := hostKeychain{
		registryHost(srcImage): &authn.Basic{Username: src.User, Password: src.Token},
		registryHost(dstImage): &authn.Basic{Username: dst.User, Password: dst.Token},
	}
	srcRef := srcImage + ":" + tag
	dstRef := dstImage + ":" + tag
	if err := crane.Copy(srcRef, dstRef, crane.WithAuthFromKeychain(kc)); err != nil {
		return fmt.Errorf("copy %s -> %s: %w", srcRef, dstRef, err)
	}
	return nil
}
