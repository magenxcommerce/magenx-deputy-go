package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// newRegistry starts an in-memory OCI registry on 127.0.0.1 (plain http) and returns its host:port.
func newRegistry(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

// pushRelease pushes archive as the only layer of repository, like "crane append -f archive", with the
// version annotation (none when version is ""), under every tag. It returns the manifest digest.
func pushRelease(t *testing.T, repository string, archive []byte, version string, tags ...string) string {
	t.Helper()
	return pushLayers(t, repository, [][]byte{archive}, version, tags...)
}

func pushLayers(t *testing.T, repository string, layers [][]byte, version string, tags ...string) string {
	t.Helper()
	img := empty.Image
	for _, b := range layers {
		var err error
		if img, err = mutate.AppendLayers(img, static.NewLayer(b, types.DockerLayer)); err != nil {
			t.Fatal(err)
		}
	}
	if version != "" {
		img = mutate.Annotations(img, map[string]string{ociVersionAnnotation: version}).(v1.Image)
	}
	for _, tag := range tags {
		ref, err := name.NewTag(repository + ":" + tag)
		if err != nil {
			t.Fatal(err)
		}
		if err := remote.Write(ref, img); err != nil {
			t.Fatal(err)
		}
	}
	d, err := img.Digest()
	if err != nil {
		t.Fatal(err)
	}
	return d.String()
}

func TestOCIResolveAndDownload(t *testing.T) {
	repo := newRegistry(t) + "/acme/magento-release"
	archive := makeTarGz(t, map[string]string{"index.php": "<?php\n"})
	manifest := pushRelease(t, repo, archive, "v1.2.0", "v1.2.0", "production")

	c := ociClient{}
	a, err := c.resolve(context.Background(), repo, "production")
	if err != nil {
		t.Fatal(err)
	}
	if a.Version != "v1.2.0" || a.LayerDigest != digestOf(archive) || a.ManifestDigest != manifest {
		t.Fatalf("resolved %+v, want version v1.2.0, layer %s, manifest %s", a, digestOf(archive), manifest)
	}
	if a.SignedRef() != repo+"@"+manifest {
		t.Fatalf("signed ref %q", a.SignedRef())
	}
	dst := filepath.Join(t.TempDir(), "release.tar.gz")
	digest, err := c.download(context.Background(), a, dst)
	if err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, dst); digest != digestOf(archive) || got != string(archive) {
		t.Fatalf("downloaded %d bytes, digest %s", len(got), digest)
	}
}

func TestOCIFetchRefusesWrongArchive(t *testing.T) {
	repo := newRegistry(t) + "/acme/magento-release"
	archive := makeTarGz(t, map[string]string{"a": "1"})
	pushRelease(t, repo, archive, "v1", "v1")
	pushLayers(t, repo, [][]byte{archive, makeTarGz(t, map[string]string{"b": "2"})}, "v2", "v2")

	c := ociClient{}
	dir := t.TempDir()
	verified := false
	verify := func(context.Context, ociArtifact) error { verified = true; return nil }
	_, err := c.fetch(context.Background(), repo, "v1", digestOf([]byte("other")), filepath.Join(dir, "x"), verify)
	if err == nil || !strings.Contains(err.Error(), "the webhook expects") || verified {
		t.Fatalf("wrong digest: err=%v verified=%v", err, verified)
	}
	if _, err := c.fetch(context.Background(), repo, "v2", "", filepath.Join(dir, "y"), nil); err == nil || !strings.Contains(err.Error(), "has 2 layers") {
		t.Fatalf("two layers: %v", err)
	}
	if _, err := c.fetch(context.Background(), repo, "missing", "", filepath.Join(dir, "z"), nil); err == nil {
		t.Fatal("missing tag fetched")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("files left after refused fetches: %v", entries)
	}
}

func TestValidateOCIRepository(t *testing.T) {
	for repo, ok := range map[string]bool{
		"ghcr.io/acme/magento-release":           true,
		"localhost:5000/acme/stack":              true,
		"":                                       false,
		"ghcr.io/acme/magento-release:latest":    false,
		"ghcr.io/acme/magento-release@sha256:00": false,
		"ghcr.io/Acme/UPPER":                     false,
	} {
		if err := validateOCIRepository("deploy.oci_repository", repo); (err == nil) != ok {
			t.Errorf("%q: err=%v, want ok=%v", repo, err, ok)
		}
	}
}

// codeOCIEnv is a code deployer with source oci, a test registry and the fake cosign.
func codeOCIEnv(t *testing.T) (*testEnv, string, string) {
	t.Helper()
	e := newTestEnv(t)
	fake := t.TempDir()
	writeFile(t, filepath.Join(fake, "cosign"), fakeCosign, 0o755)
	writeFile(t, filepath.Join(fake, "cosign.pub"), "-----BEGIN PUBLIC KEY-----\n", 0o644)
	repo := newRegistry(t) + "/acme/magento-release"
	cfg := e.deployer.cfg
	cfg.Deploy.Source = sourceOCI
	cfg.Deploy.OCIRepository = repo
	cfg.Deploy.CosignBinary = filepath.Join(fake, "cosign")
	cfg.Deploy.CosignPublicKey = filepath.Join(fake, "cosign.pub")
	if err := cfg.validate(); err != nil {
		t.Fatal(err)
	}
	id, err := currentIdentity()
	if err != nil {
		t.Fatal(err)
	}
	e.deployer = &S3MagentoDeployer{cfg: cfg, run: newRunner(id, nil), sigRun: newRunner(id, []string{"FAKE_DIR=" + fake}), oci: ociClient{}}
	if err := e.deployer.checkTools(); err != nil {
		t.Fatal(err)
	}
	return e, repo, fake
}

func TestCodeDeployFromOCI(t *testing.T) {
	e, repo, fake := codeOCIEnv(t)
	archive := makeTarGz(t, map[string]string{"index.php": "v1"})
	manifest := pushRelease(t, repo, archive, "v1.0.0", "v1.0.0")

	payload := DeployPayload{Tag: "v1.0.0", ArtifactDigest: digestOf(archive)}
	s := &Server{cfg: e.deployer.cfg}
	if msg := s.validateCodePayload(&payload); msg != "" {
		t.Fatal(msg)
	}
	if payload.ArtifactName != "release_v1.0.0.tar.gz" {
		t.Fatalf("artifact_name=%q", payload.ArtifactName)
	}
	if err := e.deployer.Deploy(context.Background(), payload); err != nil {
		t.Fatal(err)
	}
	if filepath.Base(e.current()) != "v1.0.0" || readFile(t, filepath.Join(e.releasesDir(), "v1.0.0", "index.php")) != "v1" {
		t.Fatalf("current=%q", e.current())
	}
	log := readFile(t, filepath.Join(fake, "log"))
	if !strings.Contains(log, "cosign verify --key "+e.deployer.cfg.Deploy.CosignPublicKey+" --insecure-ignore-tlog=true "+repo+"@"+manifest) {
		t.Fatalf("manifest signature not verified\n%s", log)
	}

	// a signature that does not verify: nothing is downloaded or switched
	writeFile(t, filepath.Join(fake, "cosign_fail"), "acme/magento-release\n", 0o644)
	archive2 := makeTarGz(t, map[string]string{"index.php": "v2"})
	pushRelease(t, repo, archive2, "v2.0.0", "v2.0.0")
	err := e.deployer.Deploy(context.Background(), DeployPayload{Tag: "v2.0.0", ArtifactName: "release_v2.0.0.tar.gz", ArtifactDigest: digestOf(archive2)})
	if err == nil || !strings.Contains(err.Error(), "signature check") || filepath.Base(e.current()) != "v1.0.0" {
		t.Fatalf("unsigned release: err=%v current=%q", err, e.current())
	}
	if names := e.releaseNames(); len(names) != 1 {
		t.Fatalf("releases left: %v", names)
	}
}

func TestCodePayloadTagNeedsOCI(t *testing.T) {
	e := newTestEnv(t)
	s := &Server{cfg: e.deployer.cfg}
	p := DeployPayload{ArtifactName: "release_1.tar.gz", ArtifactDigest: digestOf([]byte("x")), Tag: "v1"}
	if msg := s.validateCodePayload(&p); !strings.Contains(msg, "deploy.source oci") {
		t.Fatalf("tag with source s3: %q", msg)
	}
	s.cfg.Deploy.Source = sourceOCI
	p = DeployPayload{ArtifactDigest: digestOf([]byte("x"))}
	if msg := s.validateCodePayload(&p); !strings.Contains(msg, "tag is required") {
		t.Fatalf("no tag with source oci: %q", msg)
	}
}

func TestStackDeployFromOCI(t *testing.T) {
	e := newStackEnv(t)
	digest := e.addRelease("v1.0.0", nil, nil)
	repo := newRegistry(t) + "/acme/stack"
	manifest := pushRelease(t, repo, e.objects[stackBundleName("v1.0.0")], "v1.0.0", "v1.0.0")
	e.deployer.cfg.Source = sourceOCI
	e.deployer.cfg.OCIRepository = repo

	if err := e.deploy("v1.0.0", digest); err != nil {
		t.Fatal(err)
	}
	if e.current() != "v1.0.0" || len(e.downloads) != 0 {
		t.Fatalf("current=%q s3 downloads=%v", e.current(), e.downloads)
	}
	log := e.log()
	if !strings.Contains(log, "cosign verify --key "+e.deployer.cfg.CosignPublicKey+" --insecure-ignore-tlog=true "+repo+"@"+manifest+"\n") ||
		strings.Contains(log, "verify-blob") {
		t.Fatalf("want the manifest signature, not verify-blob\n%s", log)
	}
}

func TestCosignGetsRegistryLoginInDockerConfig(t *testing.T) {
	fake := t.TempDir()
	// records the arguments and the docker config cosign sees, then whether the config is still there later
	script := `#!/bin/sh
echo "args $*" >> "$FAKE_DIR/log"
echo "$DOCKER_CONFIG" > "$FAKE_DIR/config_dir"
cat "$DOCKER_CONFIG/config.json" > "$FAKE_DIR/config.json"
`
	writeFile(t, filepath.Join(fake, "cosign"), script, 0o755)
	t.Setenv("DEPUTY_TEST_REGISTRY_PASSWORD", "s3cret-token")
	c := ociClient{cfg: OCIConfig{Username: "deploy", LoginEnv: "DEPUTY_TEST_REGISTRY_PASSWORD"}}
	id, err := currentIdentity()
	if err != nil {
		t.Fatal(err)
	}
	a := ociArtifact{Repository: "ghcr.io/acme/stack", ManifestDigest: digestOf([]byte("m"))}
	if err := c.cosignVerifyManifest(context.Background(), newRunner(id, []string{"FAKE_DIR=" + fake}), filepath.Join(fake, "cosign"), "/k.pub", nil, a); err != nil {
		t.Fatal(err)
	}
	if args := readFile(t, filepath.Join(fake, "log")); strings.Contains(args, "s3cret") {
		t.Fatalf("password on the command line: %s", args)
	}
	var cfg struct {
		Auths map[string]struct{ Auth string } `json:"auths"`
	}
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(fake, "config.json"))), &cfg); err != nil {
		t.Fatal(err)
	}
	if got := cfg.Auths["ghcr.io"].Auth; got != base64.StdEncoding.EncodeToString([]byte("deploy:s3cret-token")) {
		t.Fatalf("auth for ghcr.io = %q", got)
	}
	if _, err := os.Stat(strings.TrimSpace(readFile(t, filepath.Join(fake, "config_dir")))); !os.IsNotExist(err) {
		t.Fatalf("docker config dir left behind: %v", err)
	}
}

// bootstrapEnv is a server with code and stack deployers, both from source oci on a test registry.
type bootstrapEnv struct {
	t      *testing.T
	cfg    Config
	server *Server
	fake   string
	codeRepo,
	stackRepo string
}

func newBootstrapEnv(t *testing.T) *bootstrapEnv {
	t.Helper()
	root := t.TempDir()
	fake := filepath.Join(root, "fake")
	shared := filepath.Join(root, "docker")
	for _, d := range []string{fake, shared} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(fake, "docker"), fakeDocker, 0o755)
	writeFile(t, filepath.Join(fake, "cosign"), fakeCosign, 0o755)
	writeFile(t, filepath.Join(fake, "cosign.pub"), "-----BEGIN PUBLIC KEY-----\n", 0o644)
	writeFile(t, filepath.Join(shared, ".env.secrets"), "SECRET=1\n", 0o600)
	host := newRegistry(t)
	e := &bootstrapEnv{t: t, fake: fake, codeRepo: host + "/acme/magento-release", stackRepo: host + "/acme/stack"}
	e.cfg = Config{
		Deploy: DeployConfig{
			RootDir: filepath.Join(root, "www"), DefaultBrand: "shop", DefaultDomain: "shop.example",
			LeaderEnvVar: "DEPUTY_TEST_LEADER_UNSET", LeaderDefault: true,
			Source: sourceOCI, OCIRepository: e.codeRepo,
			CosignBinary: filepath.Join(fake, "cosign"), CosignPublicKey: filepath.Join(fake, "cosign.pub"),
		},
		Stack: StackConfig{
			Enabled: true, Dir: filepath.Join(root, "stack"), Source: sourceOCI, OCIRepository: e.stackRepo,
			DockerBinary: filepath.Join(fake, "docker"), CosignBinary: filepath.Join(fake, "cosign"),
			CosignPublicKey: filepath.Join(fake, "cosign.pub"), ExcludeServices: []string{"magento"},
			SharedLinks: []SharedTarget{{Source: filepath.Join(shared, ".env.secrets"), Target: ".env.secrets"}},
		},
	}
	e.cfg.applyDefaults()
	e.cfg.Deploy.MinimumDiskSpaceGB = 0
	e.cfg.Stack.MinimumDiskSpaceGB = 0
	if err := e.cfg.validate(); err != nil {
		t.Fatal(err)
	}
	e.build()
	return e
}

// build creates the deployers from e.cfg, like buildServer with the fake tools.
func (e *bootstrapEnv) build() {
	id, err := currentIdentity()
	if err != nil {
		e.t.Fatal(err)
	}
	run := newRunner(id, []string{"FAKE_DIR=" + e.fake})
	code := &S3MagentoDeployer{cfg: e.cfg, run: run, sigRun: run, oci: ociClient{cfg: e.cfg.OCI}}
	e.server = &Server{cfg: e.cfg, deployer: code, stack: newStackDeployer(e.cfg, run)}
}

// pushBoth pushes a stack bundle and a code archive of version under the version and the extra tags.
func (e *bootstrapEnv) pushBoth(version string, tags ...string) (stackManifest, codeManifest string) {
	e.t.Helper()
	bundle := makeBundle(e.t, map[string]string{
		"./docker-compose.yml":         "services: {}\n",
		"./.env":                       "BRAND=shop\n",
		"./docker-compose.release.yml": "services:\n  php:\n    image: ghcr.io/acme/shop-php@sha256:" + strings.Repeat("a", 64) + "\n",
	})
	code := makeTarGz(e.t, map[string]string{"index.php": version})
	all := append([]string{version}, tags...)
	return pushRelease(e.t, e.stackRepo, bundle, version, all...), pushRelease(e.t, e.codeRepo, code, version, all...)
}

func (e *bootstrapEnv) codeCurrent() string { return currentCodeRelease(e.cfg) }

func (e *bootstrapEnv) stackCurrent() string {
	target, _ := os.Readlink(filepath.Join(e.cfg.Stack.Dir, "current"))
	return filepath.Base(target)
}

func TestBootstrapDeploysStackThenCode(t *testing.T) {
	e := newBootstrapEnv(t)
	stackManifest, codeManifest := e.pushBoth("v1.2.0", "production")

	if err := bootstrap(context.Background(), e.cfg, e.server, bootstrapOptions{StackTag: "production", CodeTag: "production"}); err != nil {
		t.Fatal(err)
	}
	if e.stackCurrent() != "v1.2.0" || e.codeCurrent() != "v1.2.0" {
		t.Fatalf("stack=%q code=%q, want the version of the annotation", e.stackCurrent(), e.codeCurrent())
	}
	log := readFile(t, filepath.Join(e.fake, "log"))
	stackCheck := strings.Index(log, e.stackRepo+"@"+stackManifest)
	up := strings.Index(log, " up --detach")
	codeCheck := strings.Index(log, e.codeRepo+"@"+codeManifest)
	if stackCheck < 0 || up < 0 || codeCheck < 0 || !(stackCheck < up && up < codeCheck) {
		t.Fatalf("want stack signature, stack up, then code signature\n%s", log)
	}

	// again, nothing new: no new code release dir, the stack is already current
	if err := bootstrap(context.Background(), e.cfg, e.server, bootstrapOptions{StackTag: "production", CodeTag: "production"}); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(filepath.Join(e.cfg.Deploy.RootDir, "shop", "releases"))
	if len(entries) != 1 {
		t.Fatalf("second bootstrap added code releases: %v", entries)
	}

	// production moves on
	e.pushBoth("v1.3.0", "production")
	if err := bootstrap(context.Background(), e.cfg, e.server, bootstrapOptions{StackTag: "production", CodeTag: "production"}); err != nil {
		t.Fatal(err)
	}
	if e.stackCurrent() != "v1.3.0" || e.codeCurrent() != "v1.3.0" {
		t.Fatalf("stack=%q code=%q after production moved", e.stackCurrent(), e.codeCurrent())
	}
}

func TestBootstrapRefusals(t *testing.T) {
	opts := bootstrapOptions{StackTag: "production", CodeTag: "production"}
	for name, tc := range map[string]struct {
		change func(*bootstrapEnv)
		want   string
	}{
		"no version annotation": {func(e *bootstrapEnv) {
			pushRelease(e.t, e.stackRepo, []byte("x"), "", "production")
		}, "annotation"},
		"unsigned bundle allowed": {func(e *bootstrapEnv) {
			e.pushBoth("v1", "production")
			e.cfg.Stack.AllowUnsignedBundle = true
		}, "allow_unsigned_bundle"},
		"stack from s3": {func(e *bootstrapEnv) {
			e.cfg.Stack.Source = sourceS3
		}, "stack.source oci"},
		"code without key": {func(e *bootstrapEnv) {
			e.pushBoth("v1", "production")
			e.cfg.Deploy.CosignPublicKey = ""
		}, "deploy.cosign_public_key"},
		"code signature fails": {func(e *bootstrapEnv) {
			e.pushBoth("v1", "production")
			writeFile(e.t, filepath.Join(e.fake, "cosign_fail"), "acme/magento-release\n", 0o644)
		}, "signature check"},
		"version tag holds another archive": {func(e *bootstrapEnv) {
			e.pushBoth("v1", "production")
			pushRelease(e.t, e.codeRepo, makeTarGz(e.t, map[string]string{"x": "other"}), "v1", "v1")
		}, "the webhook expects"},
	} {
		t.Run(name, func(t *testing.T) {
			e := newBootstrapEnv(t)
			tc.change(e)
			e.build()
			err := bootstrap(context.Background(), e.cfg, e.server, opts)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v, want %q", err, tc.want)
			}
			if e.codeCurrent() != "" {
				t.Fatalf("code switched to %q", e.codeCurrent())
			}
		})
	}
}

func TestBootstrapSkipStack(t *testing.T) {
	e := newBootstrapEnv(t)
	e.pushBoth("v1", "production")
	if err := bootstrap(context.Background(), e.cfg, e.server, bootstrapOptions{CodeTag: "production", SkipStack: true}); err != nil {
		t.Fatal(err)
	}
	if e.stackCurrent() != "." && e.stackCurrent() != "" || e.codeCurrent() != "v1" {
		t.Fatalf("stack=%q code=%q", e.stackCurrent(), e.codeCurrent())
	}
}
