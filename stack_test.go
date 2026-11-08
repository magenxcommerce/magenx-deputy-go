package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const fakeDocker = `#!/bin/sh
echo "docker $*" >> "$FAKE_DIR/log"
if [ -s "$FAKE_DIR/docker_fail" ] && echo "$*" | grep -Eq -f "$FAKE_DIR/docker_fail"; then
  echo "docker failed: $*" >&2
  exit 1
fi
case "$*" in
  *"config --format json"*) echo '{"services":{"php":{},"nginx":{"profiles":null},"magento":{},"otel":{"profiles":["observability"]}}}' ;;
esac
exit 0
`

const fakeCosign = `#!/bin/sh
echo "cosign $*" >> "$FAKE_DIR/log"
if [ -s "$FAKE_DIR/cosign_fail" ] && echo "$*" | grep -Eq -f "$FAKE_DIR/cosign_fail"; then
  echo "no matching signatures" >&2
  exit 1
fi
exit 0
`

type fakeRelease struct {
	draft, prerelease bool
	// images: service -> image reference for the lock asset
	images map[string]string
	files  map[string]string
}

type stackEnv struct {
	t        *testing.T
	dir      string
	fake     string
	shared   string
	gh       *httptest.Server
	deployer *StackDeployer

	mu       sync.Mutex
	releases map[string]fakeRelease
	auth     []string
	requests int
}

func digestImage(svc, seed string) string {
	return fmt.Sprintf("ghcr.io/acme/shop-%s@sha256:%064x", svc, len(seed)*7919+len(svc))
}

func pinned(seed string) map[string]string {
	images := map[string]string{}
	for _, svc := range []string{"php", "nginx", "magento"} {
		images[svc] = fmt.Sprintf("ghcr.io/acme/shop-%s@sha256:%s", svc, strings.Repeat(fmt.Sprintf("%x", len(seed)%16), 64))
	}
	return images
}

func newStackEnv(t *testing.T) *stackEnv {
	t.Helper()
	root := t.TempDir()
	e := &stackEnv{
		t:        t,
		dir:      filepath.Join(root, "stack"),
		fake:     filepath.Join(root, "fake"),
		shared:   filepath.Join(root, "shared"),
		releases: map[string]fakeRelease{},
	}
	for _, d := range []string{e.fake, e.shared} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	docker := filepath.Join(e.fake, "docker")
	cosign := filepath.Join(e.fake, "cosign")
	writeFile(t, docker, fakeDocker, 0o755)
	writeFile(t, cosign, fakeCosign, 0o755)
	writeFile(t, filepath.Join(e.fake, "cosign.pub"), "-----BEGIN PUBLIC KEY-----\n", 0o644)
	writeFile(t, filepath.Join(e.shared, ".env.secrets"), "SECRET=1\n", 0o600)

	e.gh = httptest.NewServer(http.HandlerFunc(e.serveGitHub))
	t.Cleanup(e.gh.Close)

	cfg := Config{
		Deploy: DeployConfig{DefaultBrand: "shop", DefaultDomain: "shop.example"},
		Stack: StackConfig{
			Enabled:         true,
			Repo:            "acme/shop-docker",
			APIURL:          e.gh.URL,
			TokenEnv:        "DEPUTY_TEST_GITHUB_TOKEN",
			Dir:             e.dir,
			DockerBinary:    docker,
			CosignBinary:    cosign,
			CosignPublicKey: filepath.Join(e.fake, "cosign.pub"),
			ExcludeServices: []string{"magento"},
			SharedLinks:     []SharedTarget{{Source: filepath.Join(e.shared, ".env.secrets"), Target: ".env.secrets"}},
		},
	}
	cfg.applyDefaults()
	cfg.Stack.MinimumDiskSpaceGB = 0
	cfg.HealthCheck.RetryIntervalSeconds = 1
	if err := cfg.validate(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DEPUTY_TEST_GITHUB_TOKEN", "test-token")
	id, err := currentIdentity()
	if err != nil {
		t.Fatal(err)
	}
	e.deployer = newStackDeployer(cfg, newRunner(id, []string{"FAKE_DIR=" + e.fake}))
	if err := e.deployer.checkTools(); err != nil {
		t.Fatal(err)
	}
	return e
}

func writeFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func (e *stackEnv) addRelease(tag string, r fakeRelease) {
	if r.images == nil {
		r.images = pinned(tag)
	}
	if r.files == nil {
		r.files = map[string]string{"docker-compose.yml": "services: {}\n", ".env": "BRAND=shop\n"}
	}
	e.mu.Lock()
	e.releases[tag] = r
	e.mu.Unlock()
}

func (e *stackEnv) serveGitHub(w http.ResponseWriter, r *http.Request) {
	e.mu.Lock()
	e.auth = append(e.auth, r.Header.Get("Authorization"))
	e.requests++
	e.mu.Unlock()

	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	lookup := func(tag string) (fakeRelease, bool) {
		e.mu.Lock()
		defer e.mu.Unlock()
		rel, ok := e.releases[tag]
		return rel, ok
	}
	switch {
	case len(parts) == 6 && parts[3] == "releases" && parts[4] == "tags":
		rel, ok := lookup(parts[5])
		if !ok {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tag_name":   parts[5],
			"draft":      rel.draft,
			"prerelease": rel.prerelease,
			"immutable":  true,
			"assets": []map[string]string{
				{"name": "docker-compose.release.yml", "url": e.gh.URL + "/assets/" + parts[5]},
			},
		})
	case len(parts) == 5 && parts[3] == "tarball":
		rel, ok := lookup(parts[4])
		if !ok {
			http.NotFound(w, r)
			return
		}
		files := map[string]string{}
		for name, content := range rel.files {
			files["acme-shop-docker-abc123/"+name] = content
		}
		_, _ = w.Write(makeTarGzT(e.t, files))
	case len(parts) == 2 && parts[0] == "assets":
		if r.Header.Get("Accept") != "application/octet-stream" {
			http.Error(w, "want octet-stream", http.StatusBadRequest)
			return
		}
		rel, ok := lookup(parts[1])
		if !ok {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintln(w, "services:")
		for _, svc := range sortedKeys(rel.images) {
			fmt.Fprintf(w, "  %s:\n    image: %s\n", svc, rel.images[svc])
		}
	default:
		http.NotFound(w, r)
	}
}

// makeTarGzT is makeTarGz with directories for nested paths, like GitHub tarballs.
func makeTarGzT(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeXGlobalHeader, Name: "pax_global_header", PAXRecords: map[string]string{"comment": "abc123"}})
	for _, name := range sortedKeys(files) {
		if dir := filepath.Dir(name); dir != "." {
			_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: dir + "/", Mode: 0o755})
		}
		content := files[name]
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content))}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write([]byte(content))
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

func (e *stackEnv) deploy(tag string, confirm ...string) error {
	return e.deployer.Deploy(context.Background(), DeployPayload{Kind: "stack", Tag: tag, Confirm: confirm})
}

func (e *stackEnv) current() string {
	target, err := os.Readlink(filepath.Join(e.dir, "current"))
	if err != nil {
		return ""
	}
	return filepath.Base(target)
}

func (e *stackEnv) log() string {
	b, _ := os.ReadFile(filepath.Join(e.fake, "log"))
	return string(b)
}

func (e *stackEnv) resetLog() { _ = os.Remove(filepath.Join(e.fake, "log")) }

func (e *stackEnv) fail(tool, pattern string) {
	writeFile(e.t, filepath.Join(e.fake, tool+"_fail"), pattern+"\n", 0o644)
}

func TestStackDeploySuccess(t *testing.T) {
	e := newStackEnv(t)
	e.addRelease("v1.0.0", fakeRelease{})

	if err := e.deploy("v1.0.0"); err != nil {
		t.Fatal(err)
	}
	if e.current() != "v1.0.0" {
		t.Fatalf("current=%q", e.current())
	}
	release := filepath.Join(e.dir, "releases", "v1.0.0")
	if got := readFile(t, filepath.Join(release, ".env")); got != "BRAND=shop\n" {
		t.Fatalf(".env=%q", got)
	}
	if got := readFile(t, filepath.Join(release, ".env.secrets")); got != "SECRET=1\n" {
		t.Fatalf(".env.secrets via shared link=%q", got)
	}
	if got := strings.TrimSpace(readFile(t, filepath.Join(release, stackTagFile))); got != "v1.0.0" {
		t.Fatalf("tag file=%q", got)
	}
	if !strings.Contains(readFile(t, filepath.Join(release, "docker-compose.release.yml")), "@sha256:") {
		t.Fatal("lock asset missing")
	}
	if _, err := os.Stat(filepath.Join(release, ".source.tar.gz")); !os.IsNotExist(err) {
		t.Fatalf("source tarball left behind: %v", err)
	}

	log := e.log()
	if n := strings.Count(log, "cosign verify --key "+e.deployer.cfg.CosignPublicKey+" --insecure-ignore-tlog=true ghcr.io/acme/"); n != 3 {
		t.Fatalf("cosign verify calls=%d\n%s", n, log)
	}
	compose := "docker compose --project-name shop --project-directory " + release +
		" -f " + filepath.Join(release, "docker-compose.yml") + " -f " + filepath.Join(release, "docker-compose.release.yml") +
		" --env-file " + filepath.Join(release, ".env") + " --env-file " + filepath.Join(release, ".env.secrets")
	for _, want := range []string{
		compose + " config --quiet",
		compose + " config --format json",
		compose + " pull --quiet nginx php\n",
		compose + " up --detach --no-build --wait --wait-timeout 600 nginx php\n",
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("docker log misses %q\n%s", want, log)
		}
	}
	if strings.Index(log, "cosign verify") > strings.Index(log, " pull ") || strings.Index(log, " pull ") > strings.Index(log, " up ") {
		t.Fatalf("wrong order: verify, pull, up\n%s", log)
	}
	for _, a := range e.auth {
		if a != "Bearer test-token" {
			t.Fatalf("authorization=%q", a)
		}
	}
}

func TestStackDeployAlreadyCurrentIsNoop(t *testing.T) {
	e := newStackEnv(t)
	e.addRelease("v1.0.0", fakeRelease{})
	if err := e.deploy("v1.0.0"); err != nil {
		t.Fatal(err)
	}
	requests := e.requests
	e.resetLog()
	if err := e.deploy("v1.0.0"); err != nil {
		t.Fatal(err)
	}
	if e.requests != requests || e.log() != "" {
		t.Fatalf("redeploy of current tag did work: requests %d->%d, log %q", requests, e.requests, e.log())
	}
}

func TestStackDeploySignatureFailureKeepsCurrent(t *testing.T) {
	e := newStackEnv(t)
	e.addRelease("v1.0.0", fakeRelease{})
	e.addRelease("v2.0.0", fakeRelease{})
	if err := e.deploy("v1.0.0"); err != nil {
		t.Fatal(err)
	}
	e.resetLog()
	e.fail("cosign", "shop-nginx")

	err := e.deploy("v2.0.0")
	if err == nil || !strings.Contains(err.Error(), "image verification failed: nginx") {
		t.Fatalf("err=%v", err)
	}
	if e.current() != "v1.0.0" {
		t.Fatalf("current=%q", e.current())
	}
	if strings.Contains(e.log(), "docker ") {
		t.Fatalf("docker ran after failed verification\n%s", e.log())
	}
	entries, _ := os.ReadDir(filepath.Join(e.dir, "releases"))
	if len(entries) != 1 {
		t.Fatalf("releases left behind: %v", entries)
	}
}

func TestStackDeployUpFailureRollsBack(t *testing.T) {
	e := newStackEnv(t)
	e.addRelease("v1.0.0", fakeRelease{})
	e.addRelease("v2.0.0", fakeRelease{})
	if err := e.deploy("v1.0.0"); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(e.fake, "rollback")
	e.deployer.cfg.RollbackCommands = []string{"echo {release_dir} > " + shellQuote(marker)}
	e.resetLog()
	e.fail("docker", "releases/v2.0.0 .* up ")

	err := e.deploy("v2.0.0")
	if err == nil || !strings.Contains(err.Error(), "up:") {
		t.Fatalf("err=%v", err)
	}
	if e.current() != "v1.0.0" {
		t.Fatalf("current=%q, want rollback to v1.0.0", e.current())
	}
	v1 := filepath.Join(e.dir, "releases", "v1.0.0")
	if !strings.Contains(e.log(), "--project-directory "+v1+" ") || !strings.Contains(e.log(), " up --detach") {
		t.Fatalf("previous release not started again\n%s", e.log())
	}
	if lines := strings.Split(strings.TrimSpace(e.log()), "\n"); !strings.Contains(lines[len(lines)-1], v1) {
		t.Fatalf("last docker call is not the rollback up of v1\n%s", e.log())
	}
	if got := strings.TrimSpace(readFile(t, marker)); got != v1 {
		t.Fatalf("rollback command release_dir=%q", got)
	}
}

func TestStackDeployPreUpFailureDoesNotSwitch(t *testing.T) {
	e := newStackEnv(t)
	e.addRelease("v1.0.0", fakeRelease{})
	e.deployer.cfg.PreUpCommands = []string{"false"}
	err := e.deploy("v1.0.0")
	if err == nil || !strings.Contains(err.Error(), "pre-up command failed") {
		t.Fatalf("err=%v", err)
	}
	if e.current() != "" || strings.Contains(e.log(), " up ") {
		t.Fatalf("switched or started after pre-up failure: current=%q\n%s", e.current(), e.log())
	}
}

func TestStackDeployRefusesReleases(t *testing.T) {
	tests := []struct {
		name    string
		release fakeRelease
		want    string
	}{
		{"draft", fakeRelease{draft: true}, "is a draft"},
		{"prerelease", fakeRelease{prerelease: true}, "is a prerelease"},
		{"unpinned image", fakeRelease{images: map[string]string{"php": "ghcr.io/acme/shop-php:1.0.0"}}, "not pinned by digest"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newStackEnv(t)
			e.addRelease("v1.0.0", tc.release)
			err := e.deploy("v1.0.0")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v want %q", err, tc.want)
			}
			if e.current() != "" || strings.Contains(e.log(), " pull ") {
				t.Fatalf("deployed anyway\n%s", e.log())
			}
		})
	}
}

func TestStackDeployMissingEnvFile(t *testing.T) {
	e := newStackEnv(t)
	e.deployer.cfg.EnvFiles = []string{".env", ".env.local"}
	e.addRelease("v1.0.0", fakeRelease{})
	err := e.deploy("v1.0.0")
	if err == nil || !strings.Contains(err.Error(), "env file .env.local") {
		t.Fatalf("err=%v", err)
	}
}

func TestStackDeployConfirmServices(t *testing.T) {
	e := newStackEnv(t)
	e.deployer.cfg.ConfirmServices = []string{"php"}
	v1 := pinned("v1")
	v2 := pinned("v1")
	v2["nginx"] = digestImage("nginx", "changed")
	v3 := pinned("v1")
	v3["php"] = "ghcr.io/acme/shop-php@sha256:" + strings.Repeat("e", 64)
	e.addRelease("v1", fakeRelease{images: v1})
	e.addRelease("v2", fakeRelease{images: v2})
	e.addRelease("v3", fakeRelease{images: v3})

	if err := e.deploy("v1"); err == nil || !strings.Contains(err.Error(), "php needs confirmation") {
		t.Fatalf("first deploy without confirm: err=%v", err)
	}
	if err := e.deploy("v1", "php"); err != nil {
		t.Fatal(err)
	}
	if err := e.deploy("v2"); err != nil {
		t.Fatalf("php unchanged, no confirmation needed: %v", err)
	}
	if err := e.deploy("v3"); err == nil || !strings.Contains(err.Error(), "needs confirmation") {
		t.Fatalf("php changed without confirm: err=%v", err)
	}
	if e.current() != "v2" {
		t.Fatalf("current=%q", e.current())
	}
	if err := e.deploy("v3", "php"); err != nil {
		t.Fatal(err)
	}
}

func TestStackDeployProfiles(t *testing.T) {
	e := newStackEnv(t)
	e.deployer.cfg.Profiles = []string{"observability"}
	e.addRelease("v1", fakeRelease{})
	if err := e.deploy("v1"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.log(), "--profile observability up --detach --no-build --wait --wait-timeout 600 nginx otel php\n") {
		t.Fatalf("profile service not started\n%s", e.log())
	}
}

func TestStackDeployPrunesReleases(t *testing.T) {
	e := newStackEnv(t)
	e.deployer.cfg.KeepReleases = 2
	for _, tag := range []string{"v1", "v2", "v3", "v4"} {
		e.addRelease(tag, fakeRelease{})
		if err := e.deploy(tag); err != nil {
			t.Fatal(err)
		}
	}
	entries, _ := os.ReadDir(filepath.Join(e.dir, "releases"))
	if len(entries) != 2 {
		t.Fatalf("releases=%v", entries)
	}
}

func TestStackPollDeploysLatestOnce(t *testing.T) {
	e := newStackEnv(t)
	e.addRelease("v1.0.0", fakeRelease{})
	latest := "v1.0.0"
	inner := e.gh.Config.Handler
	e.gh.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/releases/latest") {
			_ = json.NewEncoder(w).Encode(map[string]any{"tag_name": latest})
			return
		}
		inner.ServeHTTP(w, r)
	})
	server := &Server{cfg: Config{Stack: e.deployer.cfg}, stack: e.deployer}
	failed := ""
	server.pollStackOnce(context.Background(), e.deployer, &failed)
	if e.current() != "v1.0.0" || failed != "" {
		t.Fatalf("current=%q failed=%q", e.current(), failed)
	}

	// a failing tag is not retried until a newer one appears
	latest = "v2.0.0"
	server.pollStackOnce(context.Background(), e.deployer, &failed)
	if failed != "v2.0.0" {
		t.Fatalf("failed=%q", failed)
	}
	// only /releases/latest (served above, not counted) may be requested
	requests := e.requests
	server.pollStackOnce(context.Background(), e.deployer, &failed)
	if e.requests != requests {
		t.Fatalf("failed tag retried: requests %d -> %d", requests, e.requests)
	}
}

func TestStackConfigValidate(t *testing.T) {
	base := func() Config {
		c := Config{
			Deploy: DeployConfig{DefaultBrand: "Shop", DefaultDomain: "shop.example"},
			Stack:  StackConfig{Enabled: true, Repo: "acme/shop", CosignPublicKey: "/etc/k.pub"},
		}
		c.applyDefaults()
		return c
	}
	c := base()
	if err := c.validate(); err != nil {
		t.Fatal(err)
	}
	if c.Stack.Dir != "/opt/Shop/stack" || c.Stack.ProjectName != "shop" {
		t.Fatalf("defaults: dir=%q project=%q", c.Stack.Dir, c.Stack.ProjectName)
	}
	for name, mutate := range map[string]func(*StackConfig){
		"no key":        func(s *StackConfig) { s.CosignPublicKey = "" },
		"bad repo":      func(s *StackConfig) { s.Repo = "acme" },
		"relative dir":  func(s *StackConfig) { s.Dir = "stack" },
		"env outside":   func(s *StackConfig) { s.EnvFiles = []string{"../.env"} },
		"bad service":   func(s *StackConfig) { s.ExcludeServices = []string{"-x"} },
		"lock path":     func(s *StackConfig) { s.LockAsset = "a/b.yml" },
		"bad health":    func(s *StackConfig) { s.HealthCheckURL = "ftp://x" },
		"upper project": func(s *StackConfig) { s.ProjectName = "Shop" },
	} {
		c := base()
		mutate(&c.Stack)
		if err := c.validate(); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}
