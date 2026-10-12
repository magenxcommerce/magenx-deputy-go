package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// makeTarGz returns a gzipped tar with the given files (name -> content).
func makeTarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

type testEnv struct {
	t        *testing.T
	root     string
	deployer *S3MagentoDeployer
	archive  []byte
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	root := t.TempDir()
	cfg := Config{
		Deploy: DeployConfig{
			RootDir:            root,
			DefaultBrand:       "demo",
			DefaultDomain:      "shop.example",
			MinimumDiskSpaceGB: 0,
			LeaderEnvVar:       "DEPUTY_TEST_LEADER_UNSET",
			LeaderDefault:      true,
		},
	}
	cfg.applyDefaults()
	cfg.Deploy.MinimumDiskSpaceGB = 0
	cfg.HealthCheck.RetryIntervalSeconds = 1
	env := &testEnv{t: t, root: root}
	env.deployer = &S3MagentoDeployer{cfg: cfg}
	env.deployer.download = func(_ context.Context, _, _, destination string) (string, error) {
		if err := os.WriteFile(destination, env.archive, 0o640); err != nil {
			return "", err
		}
		return digestOf(env.archive), nil
	}
	return env
}

func (e *testEnv) deploy(artifact string, files map[string]string) error {
	e.archive = makeTarGz(e.t, files)
	return e.deployer.Deploy(context.Background(), DeployPayload{
		ArtifactName:   artifact,
		ArtifactDigest: digestOf(e.archive),
		Brand:          "demo",
		Domain:         "shop.example",
	})
}

func (e *testEnv) releasesDir() string { return filepath.Join(e.root, "demo", "releases") }
func (e *testEnv) currentLink() string { return filepath.Join(e.root, "demo", "public", "current") }

func (e *testEnv) current() string {
	e.t.Helper()
	target, err := os.Readlink(e.currentLink())
	if err != nil {
		return ""
	}
	return target
}

func (e *testEnv) releaseNames() []string {
	e.t.Helper()
	entries, err := os.ReadDir(e.releasesDir())
	if err != nil {
		e.t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestDeploySuccessSwitchesCurrent(t *testing.T) {
	env := newTestEnv(t)
	if err := env.deploy("release_v1.0.0.tar.gz", map[string]string{"index.php": "v1"}); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(env.releasesDir(), "v1.0.0")
	if env.current() != want {
		t.Fatalf("current=%q want=%q", env.current(), want)
	}
	if got := readFile(t, filepath.Join(want, "index.php")); got != "v1" {
		t.Fatalf("index.php=%q", got)
	}
	if names := env.releaseNames(); len(names) != 1 {
		t.Fatalf("releases=%v, staging dir left behind?", names)
	}
}

func TestDeployRedeploySameVersionDoesNotTouchLiveRelease(t *testing.T) {
	env := newTestEnv(t)
	if err := env.deploy("release_v1.0.0.tar.gz", map[string]string{"index.php": "first"}); err != nil {
		t.Fatal(err)
	}
	live := env.current()
	if err := env.deploy("release_v1.0.0.tar.gz", map[string]string{"index.php": "second"}); err != nil {
		t.Fatal(err)
	}
	if env.current() == live {
		t.Fatal("redeploy reused the live release dir")
	}
	if got := readFile(t, filepath.Join(live, "index.php")); got != "first" {
		t.Fatalf("live release modified: %q", got)
	}
	if got := readFile(t, filepath.Join(env.current(), "index.php")); got != "second" {
		t.Fatalf("new release content=%q", got)
	}
}

func TestDeployLeaderCommandFailureRollsBack(t *testing.T) {
	env := newTestEnv(t)
	if err := env.deploy("release_v1.0.0.tar.gz", map[string]string{"index.php": "v1"}); err != nil {
		t.Fatal(err)
	}
	previous := env.current()
	marker := filepath.Join(env.root, "rollback-marker")
	env.deployer.cfg.Deploy.LeaderCommands = []string{"exit 3"}
	env.deployer.cfg.Deploy.RollbackCommands = []string{"echo {release_dir} > " + shellQuote(marker)}

	err := env.deploy("release_v2.0.0.tar.gz", map[string]string{"index.php": "v2"})
	if err == nil || !strings.Contains(err.Error(), "leader command failed") {
		t.Fatalf("err=%v", err)
	}
	if env.current() != previous {
		t.Fatalf("current=%q want previous %q", env.current(), previous)
	}
	if got := strings.TrimSpace(readFile(t, marker)); got != previous {
		t.Fatalf("rollback command got release_dir=%q want %q", got, previous)
	}
}

func TestDeployRollbackCommandFailureIsReported(t *testing.T) {
	env := newTestEnv(t)
	if err := env.deploy("release_v1.0.0.tar.gz", map[string]string{"index.php": "v1"}); err != nil {
		t.Fatal(err)
	}
	env.deployer.cfg.Deploy.LeaderCommands = []string{"false"}
	env.deployer.cfg.Deploy.RollbackCommands = []string{"echo flush-broken >&2; exit 1"}

	err := env.deploy("release_v2.0.0.tar.gz", map[string]string{"index.php": "v2"})
	if err == nil || !strings.Contains(err.Error(), "rollback command failed") || !strings.Contains(err.Error(), "flush-broken") {
		t.Fatalf("err=%v", err)
	}
}

func TestDeployFirstReleaseFailureRemovesCurrentLink(t *testing.T) {
	env := newTestEnv(t)
	env.deployer.cfg.Deploy.LeaderCommands = []string{"false"}
	if err := env.deploy("release_v1.0.0.tar.gz", map[string]string{"index.php": "v1"}); err == nil {
		t.Fatal("expected failure")
	}
	if _, err := os.Lstat(env.currentLink()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("current link still present: %v", err)
	}
}

func TestDeployPreSwitchRunsInNewReleaseBeforeSwitch(t *testing.T) {
	env := newTestEnv(t)
	if err := env.deploy("release_v1.0.0.tar.gz", map[string]string{"index.php": "v1"}); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(env.root, "pre-switch")
	// records the release dir and what current pointed to while the command ran
	env.deployer.cfg.Deploy.PreSwitchCommands = []string{
		"cd {release_dir} && pwd -P > " + shellQuote(marker) + " && readlink " + shellQuote(env.currentLink()) + " >> " + shellQuote(marker) + " && test \"$RELEASE_DIR\" = \"$(pwd)\"",
	}
	previous := env.current()
	if err := env.deploy("release_v2.0.0.tar.gz", map[string]string{"index.php": "v2"}); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(readFile(t, marker)), "\n")
	realNew, _ := filepath.EvalSymlinks(env.current())
	if len(lines) != 2 || lines[0] != realNew || lines[1] != previous {
		t.Fatalf("pre-switch saw %q, want [%q %q]", lines, realNew, previous)
	}
}

func TestDeployPreSwitchFailureKeepsCurrentAndCleansUp(t *testing.T) {
	env := newTestEnv(t)
	if err := env.deploy("release_v1.0.0.tar.gz", map[string]string{"index.php": "v1"}); err != nil {
		t.Fatal(err)
	}
	previous := env.current()
	env.deployer.cfg.Deploy.PreSwitchCommands = []string{"false"}

	err := env.deploy("release_v2.0.0.tar.gz", map[string]string{"index.php": "v2"})
	if err == nil || !strings.Contains(err.Error(), "pre-switch command failed") {
		t.Fatalf("err=%v", err)
	}
	if env.current() != previous {
		t.Fatalf("current changed to %q", env.current())
	}
	if names := env.releaseNames(); len(names) != 1 || names[0] != "v1.0.0" {
		t.Fatalf("releases=%v", names)
	}
}

func TestDeployExtractFailureLeavesNoStagingDir(t *testing.T) {
	env := newTestEnv(t)
	if err := env.deploy("release_v1.0.0.tar.gz", map[string]string{"index.php": "v1"}); err != nil {
		t.Fatal(err)
	}
	env.archive = []byte("not a tarball")
	err := env.deployer.Deploy(context.Background(), DeployPayload{
		ArtifactName:   "release_v2.0.0.tar.gz",
		ArtifactDigest: digestOf(env.archive),
		Brand:          "demo",
	})
	if err == nil {
		t.Fatal("expected extract failure")
	}
	if names := env.releaseNames(); len(names) != 1 || names[0] != "v1.0.0" {
		t.Fatalf("releases=%v", names)
	}
}

func TestDeployDigestMismatchLeavesNoStagingDir(t *testing.T) {
	env := newTestEnv(t)
	env.archive = makeTarGz(t, map[string]string{"index.php": "v1"})
	err := env.deployer.Deploy(context.Background(), DeployPayload{
		ArtifactName:   "release_v1.0.0.tar.gz",
		ArtifactDigest: "sha256:" + strings.Repeat("0", 64),
		Brand:          "demo",
	})
	if err == nil || !strings.Contains(err.Error(), "artifact digest mismatch") {
		t.Fatalf("err=%v", err)
	}
	if names := env.releaseNames(); len(names) != 0 {
		t.Fatalf("releases=%v", names)
	}
}

func TestDeployPrunesOldReleases(t *testing.T) {
	env := newTestEnv(t)
	env.deployer.cfg.Deploy.KeepReleases = 2
	for i := 1; i <= 4; i++ {
		name := "release_v" + strconv.Itoa(i) + ".0.0.tar.gz"
		if err := env.deploy(name, map[string]string{"index.php": name}); err != nil {
			t.Fatal(err)
		}
		// distinct mtimes for ordering
		dir := env.current()
		ts := time.Now().Add(-time.Duration(10-i) * time.Minute)
		if err := os.Chtimes(dir, ts, ts); err != nil {
			t.Fatal(err)
		}
	}
	names := env.releaseNames()
	if strings.Join(names, ",") != "v3.0.0,v4.0.0" {
		t.Fatalf("releases=%v", names)
	}
}

func TestPruneReleasesNeverRemovesProtected(t *testing.T) {
	dir := t.TempDir()
	base := time.Now()
	for i, name := range []string{"old", "mid", "new"} {
		path := filepath.Join(dir, name)
		if err := os.Mkdir(path, 0o750); err != nil {
			t.Fatal(err)
		}
		ts := base.Add(time.Duration(i) * time.Minute)
		if err := os.Chtimes(path, ts, ts); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, ".v9.tmp-123"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := pruneReleases(dir, 1, filepath.Join(dir, "new"), filepath.Join(dir, "old")); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if strings.Join(names, ",") != "new,old" {
		t.Fatalf("kept=%v", names)
	}
	if err := pruneReleases(dir, -1); err != nil {
		t.Fatal(err)
	}
}

func TestDeployHealthCheckRetriesThenRollsBack(t *testing.T) {
	var calls atomic.Int32
	failFirst := int32(1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) <= failFirst {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)

	env := newTestEnv(t)
	env.deployer.cfg.HealthCheck.Enabled = true
	env.deployer.cfg.HealthCheck.Scheme = "http"
	env.deployer.cfg.HealthCheck.Attempts = 2
	deploy := func(artifact string) error {
		env.archive = makeTarGz(t, map[string]string{"index.php": artifact})
		return env.deployer.Deploy(context.Background(), DeployPayload{
			ArtifactName: artifact, ArtifactDigest: digestOf(env.archive), Brand: "demo", Domain: u.Host,
		})
	}

	// first attempt 503, second 200 -> success
	if err := deploy("release_v1.0.0.tar.gz"); err != nil {
		t.Fatalf("expected success after retry: %v", err)
	}
	previous := env.current()

	// always failing -> rollback after 2 attempts
	calls.Store(0)
	failFirst = 100
	err := deploy("release_v2.0.0.tar.gz")
	if err == nil || !strings.Contains(err.Error(), "health check returned 503") {
		t.Fatalf("err=%v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("health check calls=%d want 2", calls.Load())
	}
	if env.current() != previous {
		t.Fatalf("current=%q want previous %q", env.current(), previous)
	}
}

type ctxDeployer struct{ ctxErr error }

func (c *ctxDeployer) Deploy(ctx context.Context, _ DeployPayload) error {
	c.ctxErr = ctx.Err()
	return nil
}

func TestWebhookDeployNotCancelledByClient(t *testing.T) {
	now := time.Now().Unix()
	payload := []byte(`{"artifact_name":"release_2.4.7.tar.gz","artifact_digest":"` + testDigest + `","timestamp":` + strconv.FormatInt(now, 10) + `}`)
	cfg := Config{
		WebhookPath:   "/deploy",
		WebhookSecret: "secret",
		HMACKey:       "hmac",
		Deploy:        DeployConfig{DefaultBrand: "demo", DefaultDomain: "shop.example"},
	}
	cfg.applyDefaults()
	deployer := &ctxDeployer{}
	server := &Server{cfg: cfg, deployer: deployer}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // client already gone
	req := httptest.NewRequest(http.MethodPost, "/deploy", bytes.NewReader(payload)).WithContext(ctx)
	req.Header.Set("X-Webhook-Secret", "secret")
	req.Header.Set("X-Signature", sign("hmac", payload))
	req.Header.Set("X-Timestamp", strconv.FormatInt(now, 10))
	res := httptest.NewRecorder()
	server.ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("status=%d", res.Code)
	}
	if deployer.ctxErr != nil {
		t.Fatalf("deploy context cancelled with client: %v", deployer.ctxErr)
	}
}

func TestShellQuote(t *testing.T) {
	out, err := os.MkdirTemp(t.TempDir(), "it's dir")
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "m")
	if err := runShellCommand(context.Background(), "cd {release_dir} && pwd > "+shellQuote(marker), out); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(readFile(t, marker)); got != out {
		t.Fatalf("pwd=%q want %q", got, out)
	}
}
