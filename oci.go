package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"

	"github.com/magenxcommerce/magenx-deputy-go/version"
)

// Release archives in an OCI registry (e.g. ghcr.io): one image manifest per release whose single layer is
// the archive itself, pushed with "crane append -f release.tar.gz", so the layer digest is the sha256 of the
// archive, the same value as artifact_digest. The release version is the manifest annotation
// org.opencontainers.image.version; bootstrap reads it to turn a moving tag (production) into the version.

const (
	sourceS3  = "s3"
	sourceOCI = "oci"
	// ociVersionAnnotation on the manifest names the release version of a moving tag.
	ociVersionAnnotation = "org.opencontainers.image.version"
)

// OCIConfig is the registry login of deputy's own pulls (archives and their signatures). docker pulls of the
// stack images use stack.registry_username instead.
type OCIConfig struct {
	// Username empty: anonymous pulls (public packages).
	Username string `json:"username" yaml:"username"`
	// LoginEnv names the environment variable with the password or token (ghcr.io: a classic PAT with
	// read:packages). Only the name is in the config, so it may appear in errors and logs; the value never does.
	LoginEnv string `json:"password_env" yaml:"password_env"`
}

// ociArtifact is a release archive resolved from repository:tag.
type ociArtifact struct {
	Repository     string
	Tag            string
	ManifestDigest string
	LayerDigest    string
	// Version is the org.opencontainers.image.version annotation, empty when the manifest has none.
	Version string
}

// SignedRef is the reference cosign verifies: the manifest by digest, never the moving tag.
func (a ociArtifact) SignedRef() string { return a.Repository + "@" + a.ManifestDigest }

type ociClient struct {
	cfg OCIConfig
	// transport replaces the default transport in tests.
	transport http.RoundTripper
}

// login is the value of oci.password_env: it only goes into the registry auth, never into errors or logs.
func (c ociClient) login() string {
	if c.cfg.LoginEnv == "" {
		return ""
	}
	return os.Getenv(c.cfg.LoginEnv)
}

func (c ociClient) options(ctx context.Context) ([]remote.Option, error) {
	opts := []remote.Option{remote.WithContext(ctx), remote.WithUserAgent("magenx-deputy-go/" + version.Version)}
	if c.transport != nil {
		opts = append(opts, remote.WithTransport(c.transport))
	}
	if c.cfg.Username != "" {
		login := c.login()
		if login == "" {
			return nil, fmt.Errorf("oci.username is set but %s is empty", c.cfg.LoginEnv)
		}
		opts = append(opts, remote.WithAuth(&authn.Basic{Username: c.cfg.Username, Password: login}))
	}
	return opts, nil
}

// resolve reads the manifest of repository:tag and requires exactly one layer: the release archive.
func (c ociClient) resolve(ctx context.Context, repository, tag string) (ociArtifact, error) {
	ref, err := name.NewTag(repository+":"+tag, name.StrictValidation)
	if err != nil {
		return ociArtifact{}, fmt.Errorf("invalid reference %s:%s: %w", repository, tag, err)
	}
	opts, err := c.options(ctx)
	if err != nil {
		return ociArtifact{}, err
	}
	desc, err := remote.Get(ref, opts...)
	if err != nil {
		return ociArtifact{}, fmt.Errorf("resolve %s: %w", ref, err)
	}
	if desc.MediaType.IsIndex() {
		return ociArtifact{}, fmt.Errorf("%s is an image index, expected one image manifest with the archive as its only layer", ref)
	}
	manifest, err := v1.ParseManifest(bytes.NewReader(desc.Manifest))
	if err != nil {
		return ociArtifact{}, fmt.Errorf("%s: parse manifest: %w", ref, err)
	}
	if len(manifest.Layers) != 1 {
		return ociArtifact{}, fmt.Errorf("%s has %d layers, expected 1: the release archive", ref, len(manifest.Layers))
	}
	layer := manifest.Layers[0]
	if layer.MediaType != "" && !layer.MediaType.IsLayer() && layer.MediaType != types.MediaType("application/gzip") {
		return ociArtifact{}, fmt.Errorf("%s: unexpected layer media type %s", ref, layer.MediaType)
	}
	return ociArtifact{
		Repository:     ref.Context().Name(),
		Tag:            tag,
		ManifestDigest: desc.Digest.String(),
		LayerDigest:    layer.Digest.String(),
		Version:        manifest.Annotations[ociVersionAnnotation],
	}, nil
}

// download writes the archive layer of a to destination (created, must not exist) and returns its sha256 as
// "sha256:<hex>".
func (c ociClient) download(ctx context.Context, a ociArtifact, destination string) (string, error) {
	ref, err := name.NewDigest(a.Repository+"@"+a.LayerDigest, name.StrictValidation)
	if err != nil {
		return "", fmt.Errorf("invalid layer reference: %w", err)
	}
	opts, err := c.options(ctx)
	if err != nil {
		return "", err
	}
	layer, err := remote.Layer(ref, opts...)
	if err != nil {
		return "", fmt.Errorf("layer %s: %w", ref, err)
	}
	// the stored bytes as they are: crane append keeps the archive, the layer digest is its sha256
	rc, err := layer.Compressed()
	if err != nil {
		return "", fmt.Errorf("download %s: %w", ref, err)
	}
	defer rc.Close()
	out, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
	if err != nil {
		return "", fmt.Errorf("create local artifact file: %w", err)
	}
	digest, err := copyWithSHA256(out, rc)
	if closeErr := out.Close(); err == nil && closeErr != nil {
		err = closeErr
	}
	if err != nil {
		return "", fmt.Errorf("download %s: %w", ref, err)
	}
	return digest, nil
}

// fetch resolves repository:tag, checks that its layer is the expected archive (when expectedDigest is
// set), runs verify on the resolved artifact (signature) and downloads the archive to destination.
func (c ociClient) fetch(ctx context.Context, repository, tag, expectedDigest, destination string, verify func(context.Context, ociArtifact) error) (string, error) {
	a, err := c.resolve(ctx, repository, tag)
	if err != nil {
		return "", err
	}
	if expectedDigest != "" && !strings.EqualFold(expectedDigest, a.LayerDigest) {
		return "", fmt.Errorf("%s:%s holds %s, the webhook expects %s", a.Repository, tag, a.LayerDigest, strings.ToLower(expectedDigest))
	}
	if verify != nil {
		if err := verify(ctx, a); err != nil {
			return "", err
		}
	}
	return c.download(ctx, a, destination)
}

// cosignVerifyManifest runs "cosign verify --key <key> <repository>@<manifest digest>" as deputy's own user.
// With oci.username set, cosign gets the registry login through a private, temporary DOCKER_CONFIG, so the
// password is never on a command line.
func (c ociClient) cosignVerifyManifest(ctx context.Context, run Runner, binary, key string, args []string, a ociArtifact) error {
	env := []string{}
	if c.cfg.Username != "" {
		dir, err := c.dockerConfig(a.Repository)
		if err != nil {
			return err
		}
		defer os.RemoveAll(dir)
		env = append(env, "DOCKER_CONFIG="+dir)
	}
	cmdArgs := append(append([]string{"verify", "--key", key}, args...), a.SignedRef())
	if err := run.Run(ctx, "", env, binary, cmdArgs...); err != nil {
		return fmt.Errorf("signature check of %s failed: %w", a.SignedRef(), err)
	}
	return nil
}

// dockerConfig writes a docker config.json with the login of the repository's registry into a new 0700
// directory and returns it. The caller removes it.
func (c ociClient) dockerConfig(repository string) (string, error) {
	repo, err := name.NewRepository(repository, name.StrictValidation)
	if err != nil {
		return "", err
	}
	login := c.login()
	if login == "" {
		return "", fmt.Errorf("oci.username is set but %s is empty", c.cfg.LoginEnv)
	}
	dir, err := os.MkdirTemp("", "deputy-docker-config-")
	if err != nil {
		return "", err
	}
	auth := base64.StdEncoding.EncodeToString([]byte(c.cfg.Username + ":" + login))
	b, err := json.Marshal(map[string]any{"auths": map[string]any{repo.RegistryStr(): map[string]string{"auth": auth}}})
	if err == nil {
		err = os.WriteFile(filepath.Join(dir, "config.json"), b, 0o600)
	}
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("write registry login for cosign: %w", err)
	}
	return dir, nil
}

// validateOCIRepository checks a repository name from the config: registry/path, no tag or digest.
func validateOCIRepository(field, repository string) error {
	if repository == "" {
		return fmt.Errorf("%s is required with source %s", field, sourceOCI)
	}
	if strings.ContainsAny(repository, "@") || strings.Contains(repository[strings.LastIndex(repository, "/")+1:], ":") {
		return fmt.Errorf("%s must be a repository without tag or digest, e.g. ghcr.io/acme/magento-release", field)
	}
	if _, err := name.NewRepository(repository, name.StrictValidation); err != nil {
		return fmt.Errorf("%s: %w", field, err)
	}
	return nil
}

func validateSource(field, source string) error {
	switch source {
	case sourceS3, sourceOCI:
		return nil
	}
	return errors.New(field + " must be s3 or oci")
}
