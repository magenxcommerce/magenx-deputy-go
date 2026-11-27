package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// watchEnv is a bootstrapEnv with a watcher on a settable clock.
func watchEnv(t *testing.T) (*bootstrapEnv, *watcher, *time.Time) {
	t.Helper()
	e := newBootstrapEnv(t)
	e.cfg.Watch = WatchConfig{}
	e.cfg.Watch.applyDefaults()
	if err := e.cfg.validate(); err != nil {
		t.Fatal(err)
	}
	e.build()
	w := newWatcher(e.cfg, e.server)
	e.server.watch = w
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	w.now = func() time.Time { return now }
	return e, w, &now
}

func (e *bootstrapEnv) cosignChecks(repo string) int {
	b, _ := os.ReadFile(filepath.Join(e.fake, "log"))
	return strings.Count(string(b), "cosign verify --key "+e.cfg.Deploy.CosignPublicKey+" --insecure-ignore-tlog=true "+repo+"@")
}

func TestWatchDeploysAndSkipsUnchanged(t *testing.T) {
	e, w, _ := watchEnv(t)
	e.pushBoth("v1.0.0", "production")

	w.checkAll(context.Background())
	if e.stackCurrent() != "v1.0.0" || e.codeCurrent() != "v1.0.0" {
		t.Fatalf("first check: stack=%q code=%q", e.stackCurrent(), e.codeCurrent())
	}
	stack, code := e.cosignChecks(e.stackRepo), e.cosignChecks(e.codeRepo)
	if stack != 1 || code != 1 {
		t.Fatalf("signature checks stack=%d code=%d, want 1 each", stack, code)
	}

	// unchanged tags: only the HEAD of each tag, no resolve, no signature check, no deploy
	w.checkAll(context.Background())
	if e.cosignChecks(e.stackRepo) != stack || e.cosignChecks(e.codeRepo) != code {
		t.Fatal("unchanged tags were deployed again")
	}

	e.pushBoth("v1.1.0", "production")
	w.checkAll(context.Background())
	if e.stackCurrent() != "v1.1.0" || e.codeCurrent() != "v1.1.0" {
		t.Fatalf("after the tag moved: stack=%q code=%q", e.stackCurrent(), e.codeCurrent())
	}
	st := w.status()
	if st.Code.Version != "v1.1.0" || st.Stack == nil || st.Stack.Version != "v1.1.0" || st.Code.Error != "" || st.Code.CheckedAt.IsZero() {
		t.Fatalf("status %+v", st)
	}
}

func TestWatchFailedReleaseNotRetriedUntilTagMoves(t *testing.T) {
	e, w, _ := watchEnv(t)
	e.pushBoth("v1.0.0", "production")
	writeFile(t, filepath.Join(e.fake, "cosign_fail"), "acme/magento-release\n", 0o644)

	w.checkAll(context.Background())
	st := w.status()
	if e.codeCurrent() != "" || st.Code.FailedVersion != "v1.0.0" || !strings.Contains(st.Code.Error, "signature check") {
		t.Fatalf("code=%q status=%+v", e.codeCurrent(), st.Code)
	}
	if e.stackCurrent() != "v1.0.0" {
		t.Fatalf("stack=%q: a failing code release must not block the stack", e.stackCurrent())
	}
	tries := e.cosignChecks(e.codeRepo)
	w.checkAll(context.Background())
	if e.cosignChecks(e.codeRepo) != tries || w.status().Code.FailedVersion != "v1.0.0" {
		t.Fatal("the failed release was tried again although the tag did not move")
	}

	// a new release on the tag is tried
	_ = os.Remove(filepath.Join(e.fake, "cosign_fail"))
	e.pushBoth("v1.0.1", "production")
	w.checkAll(context.Background())
	if st := w.status(); e.codeCurrent() != "v1.0.1" || st.Code.Error != "" || st.Code.FailedVersion != "" {
		t.Fatalf("code=%q status=%+v", e.codeCurrent(), st.Code)
	}
}

func TestWatchSettleForFollowers(t *testing.T) {
	e, w, now := watchEnv(t)
	e.cfg.Deploy.LeaderDefault = false
	e.cfg.Watch.SettleSeconds = 600
	w.cfg = e.cfg
	e.pushBoth("v1.0.0", "production")

	w.checkAll(context.Background())
	if e.stackCurrent() != "v1.0.0" || e.codeCurrent() != "" {
		t.Fatalf("stack=%q code=%q: a follower waits settle_seconds for the code", e.stackCurrent(), e.codeCurrent())
	}
	*now = now.Add(599 * time.Second)
	w.checkAll(context.Background())
	if e.codeCurrent() != "" {
		t.Fatal("code deployed before settle_seconds passed")
	}
	*now = now.Add(2 * time.Second)
	w.checkAll(context.Background())
	if e.codeCurrent() != "v1.0.0" {
		t.Fatalf("code=%q after settle_seconds", e.codeCurrent())
	}

	// the leader does not wait
	e2, w2, _ := watchEnv(t)
	e2.cfg.Watch.SettleSeconds = 600
	w2.cfg = e2.cfg
	e2.pushBoth("v1.0.0", "production")
	w2.checkAll(context.Background())
	if e2.codeCurrent() != "v1.0.0" {
		t.Fatalf("leader code=%q", e2.codeCurrent())
	}
}

func TestWatchBacksOffOnRateLimit(t *testing.T) {
	var heads atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/" {
			w.WriteHeader(http.StatusOK)
			return
		}
		heads.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)
	cfg := Config{Watch: WatchConfig{IntervalSeconds: 300}}
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	w := &watcher{cfg: cfg, now: func() time.Time { return now }}
	target := &watchTarget{name: "code", repository: strings.TrimPrefix(srv.URL, "http://") + "/acme/release", tag: "production"}

	w.check(context.Background(), target)
	if target.backoff != 1 || !target.nextCheck.Equal(now.Add(600*time.Second)) || !strings.Contains(target.status.Error, "rate limit") {
		t.Fatalf("backoff=%d next=%s error=%q", target.backoff, target.nextCheck, target.status.Error)
	}
	before := heads.Load()
	now = now.Add(599 * time.Second)
	w.check(context.Background(), target)
	if heads.Load() != before {
		t.Fatal("checked the registry again before the backoff ended")
	}
	now = now.Add(2 * time.Second)
	w.check(context.Background(), target)
	if heads.Load() == before || target.backoff != 2 || !target.nextCheck.Equal(now.Add(1200*time.Second)) {
		t.Fatalf("second 429: backoff=%d next=%s", target.backoff, target.nextCheck)
	}
}

func TestWatchEndpoints(t *testing.T) {
	e, w, now := watchEnv(t)
	e.pushBoth("v1.0.0", "production")
	w.checkAll(context.Background())

	do := func(method, path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		e.server.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
		return rec
	}
	if rec := do(http.MethodPost, "/check"); rec.Code != http.StatusAccepted || len(w.nudge) != 1 {
		t.Fatalf("check: %d, queued %d", rec.Code, len(w.nudge))
	}
	if rec := do(http.MethodPost, "/check"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second check within 10s: %d", rec.Code)
	}
	*now = now.Add(11 * time.Second)
	if rec := do(http.MethodPost, "/check"); rec.Code != http.StatusAccepted || len(w.nudge) != 1 {
		t.Fatalf("check after 10s: %d, queued %d (one queued check is enough)", rec.Code, len(w.nudge))
	}
	if rec := do(http.MethodGet, "/check"); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /check: %d", rec.Code)
	}

	rec := do(http.MethodGet, "/status")
	var st watchStatus
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &st) != nil {
		t.Fatalf("status: %d %s", rec.Code, rec.Body)
	}
	if st.Code.Version != "v1.0.0" || st.Code.Tag != "production" || st.Stack == nil || st.Stack.Version != "v1.0.0" {
		t.Fatalf("status %s", rec.Body)
	}
	// there is no deploy request
	if rec := do(http.MethodPost, "/deploy"); rec.Code != http.StatusNotFound {
		t.Fatalf("POST /deploy: %d", rec.Code)
	}
}

func TestWatchConfigValidate(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(*Config)
		want   string
	}{
		"ok":                 {func(*Config) {}, ""},
		"endpoints disabled": {func(c *Config) { c.Watch.CheckPath, c.Watch.StatusPath = "-", "-" }, ""},
		"interval too short": {func(c *Config) { c.Watch.IntervalSeconds = 10 }, "at least 30"},
		"code without key":   {func(c *Config) { c.Deploy.CosignPublicKey = "" }, "cosign_public_key"},
		"bad tag":            {func(c *Config) { c.Watch.CodeTag = "../x" }, "watch.code_tag"},
		"relative path":      {func(c *Config) { c.Watch.CheckPath = "check" }, "must start with /"},
		"same paths":         {func(c *Config) { c.Watch.StatusPath = "/check" }, "must differ"},
	} {
		t.Run(name, func(t *testing.T) {
			e := newBootstrapEnv(t)
			cfg := e.cfg
			cfg.Watch = WatchConfig{}
			cfg.Watch.applyDefaults()
			tc.change(&cfg)
			err := cfg.validate()
			if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("err=%v, want %q", err, tc.want)
			}
		})
	}
}

func TestConfigListens(t *testing.T) {
	c := Config{}
	c.Watch.applyDefaults()
	if !c.listens() {
		t.Fatal("watch with /check and /status needs listen_addr")
	}
	c.Watch.CheckPath = "-"
	if !c.listens() {
		t.Fatal("/status alone needs listen_addr")
	}
	c.Watch.StatusPath = "-"
	if c.listens() {
		t.Fatal("watch without endpoints must not listen")
	}
}

func TestLockDeploysAcrossServers(t *testing.T) {
	config := filepath.Join(t.TempDir(), "config.yaml")
	writeFile(t, config, "listen_addr: 127.0.0.1:0\n", 0o444)
	// two servers stand for two processes (service and bootstrap): the flock on the config file, not the mutex,
	// serializes them; the file is read-only
	a, b := &Server{lockPath: config}, &Server{lockPath: config}
	unlock, err := a.lockDeploys()
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan struct{})
	go func() {
		u, err := b.lockDeploys()
		if err == nil {
			u()
		}
		close(got)
	}()
	select {
	case <-got:
		t.Fatal("second server got the lock while the first held it")
	case <-time.After(200 * time.Millisecond):
	}
	unlock()
	select {
	case <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("second server did not get the lock after it was released")
	}
	// without a lock path only the mutex: nothing is created anywhere
	c := &Server{}
	u, err := c.lockDeploys()
	if err != nil {
		t.Fatal(err)
	}
	u()
}

func TestWatchRunChecksAtStartAndOnNudge(t *testing.T) {
	e, w, _ := watchEnv(t)
	w.now = time.Now
	e.pushBoth("v1.0.0", "production")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.run(ctx); close(done) }()

	waitFor := func(version string) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for e.codeCurrent() != version {
			if time.Now().After(deadline) {
				t.Fatalf("code=%q, want %s", e.codeCurrent(), version)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	waitFor("v1.0.0") // the check at start

	// interval is 300s: only the nudge can deploy the moved tag within the test
	e.pushBoth("v1.1.0", "production")
	w.nudge <- struct{}{}
	waitFor("v1.1.0")

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return after its context ended")
	}
}
