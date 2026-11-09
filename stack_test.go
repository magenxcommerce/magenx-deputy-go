package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
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

type stackEnv struct {
	t        *testing.T
	dir      string
	fake     string
	shared   string
	deployer *StackDeployer

	mu        sync.Mutex
	objects   map[string][]byte // S3 objects of the stack prefix
	downloads []string
}

// pinned returns lock images for php, nginx and magento; seed changes the digests.
func pinned(seed int) map[string]string {
	images := map[string]string{}
	for _, svc := range []string{"php", "nginx", "magento"} {
		images[svc] = fmt.Sprintf("ghcr.io/acme/shop-%s@sha256:%064x", svc, seed)
	}
	return images
}

func newStackEnv(t *testing.T) *stackEnv {
	t.Helper()
	root := t.TempDir()
	e := &stackEnv{
		t:       t,
		dir:     filepath.Join(root, "stack"),
		fake:    filepath.Join(root, "fake"),
		shared:  filepath.Join(root, "docker"),
		objects: map[string][]byte{},
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

	cfg := Config{
		Deploy: DeployConfig{DefaultBrand: "shop", DefaultDomain: "shop.example", S3BucketPerBrand: true},
		Stack: StackConfig{
			Enabled:         true,
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
	id, err := currentIdentity()
	if err != nil {
		t.Fatal(err)
	}
	e.deployer = newStackDeployer(cfg, newRunner(id, []string{"FAKE_DIR=" + e.fake}))
	e.deployer.download = e.fakeS3
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

func (e *stackEnv) fakeS3(_ context.Context, name, destination string) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.downloads = append(e.downloads, name)
	b, ok := e.objects[name]
	if !ok {
		return "", fmt.Errorf("download artifact s3://shop/stack/%s: NoSuchKey", name)
	}
	if err := os.WriteFile(destination, b, 0o640); err != nil {
		return "", err
	}
	return digestOf(b), nil
}

// addRelease uploads stack_<tag>.tar.gz (repo files + lock) and its signature, returns the bundle digest.
func (e *stackEnv) addRelease(tag string, images map[string]string, files map[string]string) string {
	e.t.Helper()
	if images == nil {
		images = pinned(1)
	}
	if files == nil {
		files = map[string]string{"docker-compose.yml": "services: {}\n", ".env": "BRAND=shop\n"}
	}
	var lock strings.Builder
	lock.WriteString("services:\n")
	for _, svc := range sortedKeys(images) {
		fmt.Fprintf(&lock, "  %s:\n    image: %s\n", svc, images[svc])
	}
	all := map[string]string{"./docker-compose.release.yml": lock.String()}
	for name, content := range files {
		all["./"+name] = content
	}
	bundle := makeBundle(e.t, all)
	e.mu.Lock()
	e.objects[stackBundleName(tag)] = bundle
	e.objects[stackBundleName(tag)+".sig"] = []byte(`{"mediaType":"application/vnd.dev.sigstore.bundle.v0.3+json"}`)
	e.mu.Unlock()
	return digestOf(bundle)
}

// makeBundle is a tar.gz like the release workflow builds it: tar -czf ... -C dir .
func makeBundle(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: "./", Mode: 0o755})
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

func (e *stackEnv) deploy(tag, digest string, confirm ...string) error {
	return e.deployer.Deploy(context.Background(), DeployPayload{Kind: "stack", Tag: tag, ArtifactDigest: digest, Confirm: confirm})
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

func (e *stackEnv) releaseCount() int {
	entries, _ := os.ReadDir(filepath.Join(e.dir, "releases"))
	return len(entries)
}

func TestStackDeploySuccess(t *testing.T) {
	e := newStackEnv(t)
	digest := e.addRelease("v1.0.0", nil, nil)

	if err := e.deploy("v1.0.0", digest); err != nil {
		t.Fatal(err)
	}
	if e.current() != "v1.0.0" {
		t.Fatalf("current=%q", e.current())
	}
	if got := strings.Join(e.downloads, " "); got != "stack_v1.0.0.tar.gz stack_v1.0.0.tar.gz.sig" {
		t.Fatalf("downloads=%q", got)
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
	entries, _ := os.ReadDir(filepath.Join(e.dir, "releases"))
	if len(entries) != 1 {
		t.Fatalf("staging or download dirs left behind: %v", entries)
	}

	log := e.log()
	key := e.deployer.cfg.CosignPublicKey
	if !strings.Contains(log, "cosign verify-blob --key "+key+" --insecure-ignore-tlog=true --bundle ") || !strings.Contains(log, "stack_v1.0.0.tar.gz.sig ") {
		t.Fatalf("bundle not verified\n%s", log)
	}
	if n := strings.Count(log, "cosign verify --key "+key+" --insecure-ignore-tlog=true ghcr.io/acme/"); n != 3 {
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
	order := []int{strings.Index(log, "verify-blob"), strings.Index(log, "cosign verify "), strings.Index(log, " pull "), strings.Index(log, " up ")}
	for i := 1; i < len(order); i++ {
		if order[i-1] > order[i] {
			t.Fatalf("wrong order: verify-blob, verify, pull, up\n%s", log)
		}
	}
}

func TestStackDeployAlreadyCurrentIsNoop(t *testing.T) {
	e := newStackEnv(t)
	digest := e.addRelease("v1.0.0", nil, nil)
	if err := e.deploy("v1.0.0", digest); err != nil {
		t.Fatal(err)
	}
	downloads := len(e.downloads)
	e.resetLog()
	if err := e.deploy("v1.0.0", digest); err != nil {
		t.Fatal(err)
	}
	if len(e.downloads) != downloads || e.log() != "" {
		t.Fatalf("redeploy of current tag did work: downloads %d->%d, log %q", downloads, len(e.downloads), e.log())
	}
}

func TestStackDeployRefusesBadBundles(t *testing.T) {
	tests := []struct {
		name  string
		setup func(e *stackEnv) string // returns the digest to send
		want  string
	}{
		{"digest mismatch", func(e *stackEnv) string {
			e.addRelease("v1", nil, nil)
			return "sha256:" + strings.Repeat("0", 64)
		}, "artifact digest mismatch"},
		{"missing bundle", func(e *stackEnv) string { return "" }, "NoSuchKey"},
		{"missing signature", func(e *stackEnv) string {
			d := e.addRelease("v1", nil, nil)
			delete(e.objects, "stack_v1.tar.gz.sig")
			return d
		}, "bundle signature"},
		{"bad signature", func(e *stackEnv) string {
			e.fail("cosign", "verify-blob")
			return e.addRelease("v1", nil, nil)
		}, "bundle signature check failed"},
		{"unsigned image", func(e *stackEnv) string {
			e.fail("cosign", "shop-nginx")
			return e.addRelease("v1", nil, nil)
		}, "image verification failed: nginx"},
		{"unpinned image", func(e *stackEnv) string {
			return e.addRelease("v1", map[string]string{"php": "ghcr.io/acme/shop-php:1.0.0"}, nil)
		}, "not pinned by digest"},
		{"no lock file", func(e *stackEnv) string {
			d := e.addRelease("v1", nil, nil)
			b := makeBundle(e.t, map[string]string{"./docker-compose.yml": "x"})
			e.objects["stack_v1.tar.gz"] = b
			_ = d
			return digestOf(b)
		}, "lock file"},
		{"escaping symlink", func(e *stackEnv) string {
			var buf bytes.Buffer
			gz := gzip.NewWriter(&buf)
			tw := tar.NewWriter(gz)
			_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeSymlink, Name: "./.env.secrets", Linkname: "../../../etc/shadow"})
			_ = tw.Close()
			_ = gz.Close()
			e.addRelease("v1", nil, nil)
			e.objects["stack_v1.tar.gz"] = buf.Bytes()
			return digestOf(buf.Bytes())
		}, "leaves the release"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newStackEnv(t)
			digest := tc.setup(e)
			err := e.deploy("v1", digest)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v want %q", err, tc.want)
			}
			if e.current() != "" || strings.Contains(e.log(), "docker ") {
				t.Fatalf("deployed anyway\n%s", e.log())
			}
			if n := e.releaseCount(); n != 0 {
				t.Fatalf("%d release dirs left behind", n)
			}
		})
	}
}

func TestStackDeployUnsignedBundleAllowed(t *testing.T) {
	e := newStackEnv(t)
	e.deployer.cfg.AllowUnsignedBundle = true
	digest := e.addRelease("v1", nil, nil)
	delete(e.objects, "stack_v1.tar.gz.sig")
	if err := e.deploy("v1", digest); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(e.log(), "verify-blob") {
		t.Fatal("bundle signature checked although allow_unsigned_bundle")
	}
}

func TestStackDeployUpFailureRollsBack(t *testing.T) {
	e := newStackEnv(t)
	d1 := e.addRelease("v1.0.0", nil, nil)
	d2 := e.addRelease("v2.0.0", nil, nil)
	if err := e.deploy("v1.0.0", d1); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(e.fake, "rollback")
	e.deployer.cfg.RollbackCommands = []string{"echo {release_dir} > " + shellQuote(marker)}
	e.resetLog()
	e.fail("docker", "releases/v2.0.0 .* up ")

	err := e.deploy("v2.0.0", d2)
	if err == nil || !strings.Contains(err.Error(), "up:") {
		t.Fatalf("err=%v", err)
	}
	if e.current() != "v1.0.0" {
		t.Fatalf("current=%q, want rollback to v1.0.0", e.current())
	}
	v1 := filepath.Join(e.dir, "releases", "v1.0.0")
	lines := strings.Split(strings.TrimSpace(e.log()), "\n")
	if last := lines[len(lines)-1]; !strings.Contains(last, "--project-directory "+v1+" ") || !strings.Contains(last, " up --detach") {
		t.Fatalf("last docker call is not the rollback up of v1\n%s", e.log())
	}
	if got := strings.TrimSpace(readFile(t, marker)); got != v1 {
		t.Fatalf("rollback command release_dir=%q", got)
	}
}

func TestStackDeployPreUpFailureDoesNotSwitch(t *testing.T) {
	e := newStackEnv(t)
	digest := e.addRelease("v1.0.0", nil, nil)
	e.deployer.cfg.PreUpCommands = []string{"false"}
	err := e.deploy("v1.0.0", digest)
	if err == nil || !strings.Contains(err.Error(), "pre-up command failed") {
		t.Fatalf("err=%v", err)
	}
	if e.current() != "" || strings.Contains(e.log(), " up ") || e.releaseCount() != 0 {
		t.Fatalf("switched, started or left a release after pre-up failure: current=%q\n%s", e.current(), e.log())
	}
}

func TestStackDeployMissingEnvFile(t *testing.T) {
	e := newStackEnv(t)
	e.deployer.cfg.EnvFiles = []string{".env", ".env.local"}
	digest := e.addRelease("v1.0.0", nil, nil)
	err := e.deploy("v1.0.0", digest)
	if err == nil || !strings.Contains(err.Error(), "env file .env.local") {
		t.Fatalf("err=%v", err)
	}
}

func TestStackDeployConfirmServices(t *testing.T) {
	e := newStackEnv(t)
	e.deployer.cfg.ConfirmServices = []string{"php"}
	v2 := pinned(1)
	v2["nginx"] = pinned(2)["nginx"]
	v3 := pinned(1)
	v3["php"] = pinned(3)["php"]
	d1 := e.addRelease("v1", pinned(1), nil)
	d2 := e.addRelease("v2", v2, nil)
	d3 := e.addRelease("v3", v3, nil)

	if err := e.deploy("v1", d1); err == nil || !strings.Contains(err.Error(), "php needs confirmation") {
		t.Fatalf("first deploy without confirm: err=%v", err)
	}
	if err := e.deploy("v1", d1, "php"); err != nil {
		t.Fatal(err)
	}
	if err := e.deploy("v2", d2); err != nil {
		t.Fatalf("php unchanged, no confirmation needed: %v", err)
	}
	if err := e.deploy("v3", d3); err == nil || !strings.Contains(err.Error(), "needs confirmation") {
		t.Fatalf("php changed without confirm: err=%v", err)
	}
	if e.current() != "v2" {
		t.Fatalf("current=%q", e.current())
	}
	if err := e.deploy("v3", d3, "php"); err != nil {
		t.Fatal(err)
	}
}

func TestStackDeployProfiles(t *testing.T) {
	e := newStackEnv(t)
	e.deployer.cfg.Profiles = []string{"observability"}
	digest := e.addRelease("v1", nil, nil)
	if err := e.deploy("v1", digest); err != nil {
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
		if err := e.deploy(tag, e.addRelease(tag, nil, nil)); err != nil {
			t.Fatal(err)
		}
	}
	if n := e.releaseCount(); n != 2 {
		t.Fatalf("releases=%d", n)
	}
}

func TestStackDeployHealthCheck(t *testing.T) {
	e := newStackEnv(t)
	var mu sync.Mutex
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.WriteHeader(status)
	}))
	defer srv.Close()
	e.deployer.cfg.HealthCheckURL = srv.URL + "/health"
	if err := e.deploy("v1", e.addRelease("v1", nil, nil)); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	status = http.StatusBadGateway
	mu.Unlock()
	err := e.deploy("v2", e.addRelease("v2", nil, nil))
	if err == nil || !strings.Contains(err.Error(), "health check returned 502") {
		t.Fatalf("err=%v", err)
	}
	if e.current() != "v1" {
		t.Fatalf("current=%q", e.current())
	}
}

func TestStackConfigValidate(t *testing.T) {
	base := func() Config {
		c := Config{
			Deploy: DeployConfig{DefaultBrand: "Shop", DefaultDomain: "shop.example", S3Bucket: "b"},
			Stack:  StackConfig{Enabled: true, CosignPublicKey: "/etc/k.pub"},
		}
		c.applyDefaults()
		return c
	}
	c := base()
	if err := c.validate(); err != nil {
		t.Fatal(err)
	}
	if c.Stack.Dir != "/opt/Shop/stack" || c.Stack.ProjectName != "shop" || c.Stack.S3Prefix != "stack" {
		t.Fatalf("defaults: dir=%q project=%q prefix=%q", c.Stack.Dir, c.Stack.ProjectName, c.Stack.S3Prefix)
	}
	for name, mutate := range map[string]func(*Config){
		"no bucket":     func(c *Config) { c.Deploy.S3Bucket = "" },
		"no key":        func(c *Config) { c.Stack.CosignPublicKey = "" },
		"relative dir":  func(c *Config) { c.Stack.Dir = "stack" },
		"env outside":   func(c *Config) { c.Stack.EnvFiles = []string{"../.env"} },
		"bad service":   func(c *Config) { c.Stack.ExcludeServices = []string{"-x"} },
		"lock path":     func(c *Config) { c.Stack.LockFile = "a/b.yml" },
		"bad health":    func(c *Config) { c.Stack.HealthCheckURL = "ftp://x" },
		"upper project": func(c *Config) { c.Stack.ProjectName = "Shop" },
	} {
		c := base()
		mutate(&c)
		if err := c.validate(); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestWebhookStackPayload(t *testing.T) {
	cfg := Config{
		WebhookPath:   "/deploy",
		WebhookSecret: "secret",
		HMACKey:       "hmac",
		Deploy:        DeployConfig{DefaultBrand: "demo", DefaultDomain: "shop.example"},
	}
	cfg.applyDefaults()
	send := func(s *Server, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/deploy", strings.NewReader(body))
		req.Header.Set("X-Webhook-Secret", "secret")
		req.Header.Set("X-Signature", sign("hmac", []byte(body)))
		req.Header.Set("X-Timestamp", strconv.FormatInt(time.Now().Unix(), 10))
		res := httptest.NewRecorder()
		s.ServeHTTP(res, req)
		return res
	}
	digest := `"artifact_digest":"` + testDigest + `"`

	code, stack := &recordingDeployer{}, &recordingDeployer{}
	disabled := &Server{cfg: cfg, deployer: code}
	if res := send(disabled, `{"kind":"stack","tag":"v1.0.0",`+digest+`}`); res.Code != http.StatusBadRequest || !strings.Contains(res.Body.String(), "not enabled") {
		t.Fatalf("stack disabled: %d %s", res.Code, res.Body)
	}

	enabled := &Server{cfg: cfg, deployer: code, stack: stack}
	for body, want := range map[string]string{
		`{"kind":"stack",` + digest + `}`:                                          "tag is required",
		`{"kind":"stack","tag":"../x",` + digest + `}`:                             "tag is required",
		`{"kind":"stack","tag":"v1"}`:                                              "artifact_digest is required",
		`{"kind":"stack","tag":"v1","artifact_name":"../a.tar.gz",` + digest + `}`: "invalid artifact_name",
		`{"kind":"stack","tag":"v1","brand":"other",` + digest + `}`:               "brand does not match",
		`{"kind":"stack","tag":"v1","confirm":["a b"],` + digest + `}`:             "invalid service name",
		`{"kind":"other","tag":"v1"}`:                                              "unknown kind",
	} {
		if res := send(enabled, body); res.Code != http.StatusBadRequest || !strings.Contains(res.Body.String(), want) {
			t.Errorf("%s: %d %q want %q", body, res.Code, res.Body, want)
		}
	}
	if len(code.calls)+len(stack.calls) != 0 {
		t.Fatalf("invalid payloads deployed: %v %v", code.calls, stack.calls)
	}

	if res := send(enabled, `{"kind":"stack","tag":"v1.0.0",`+digest+`,"confirm":["mariadb"]}`); res.Code != http.StatusOK {
		t.Fatalf("stack: %d %s", res.Code, res.Body)
	}
	got := stack.calls[0]
	if len(stack.calls) != 1 || got.Tag != "v1.0.0" || got.ArtifactName != "stack_v1.0.0.tar.gz" || got.Confirm[0] != "mariadb" || len(code.calls) != 0 {
		t.Fatalf("stack calls=%v code calls=%v", stack.calls, code.calls)
	}
}

func TestStackDeployRefusesUnsafeNames(t *testing.T) {
	e := newStackEnv(t)
	for _, p := range []DeployPayload{
		{Tag: "../v1"},
		{Tag: "v1..2"},
		{Tag: ".hidden"},
		{Tag: "v1", ArtifactName: "../stack_v1.tar.gz"},
		{Tag: "v1", ArtifactName: "stack_..tar.gz"},
	} {
		p.Kind = "stack"
		if err := e.deployer.Deploy(context.Background(), p); err == nil || !strings.Contains(err.Error(), "invalid") {
			t.Errorf("%+v: err=%v", p, err)
		}
	}
	if len(e.downloads) != 0 || e.releaseCount() != 0 {
		t.Fatalf("refused payloads downloaded %v or left releases", e.downloads)
	}
}
