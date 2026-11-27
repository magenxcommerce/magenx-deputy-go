package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/go-containerregistry/pkg/v1/remote/transport"

	"github.com/magenxcommerce/magenx-deputy-go/version"
)

// Watch deploys what the moving tags (production, staging) point to, without a webhook: every interval deputy
// asks the registry for the digest of each tag (one HEAD request), and only when it changed it resolves the
// release and runs the same deploy as bootstrap: the stack first, then the Magento code. A digest whose deploy
// failed is not tried again until the tag moves (or deputy restarts). POST <check_path> asks for a check now,
// e.g. from the pipeline right after it moved the tag; GET <status_path> reports what runs.

// WatchConfig is the watch section of the config.
type WatchConfig struct {
	Enabled bool `json:"enabled" yaml:"enabled"`
	// IntervalSeconds between checks, plus up to 10% random jitter. Default 300, at least 30.
	IntervalSeconds int64 `json:"interval_seconds" yaml:"interval_seconds"`
	// StackTag and CodeTag are the moving tags in stack.oci_repository and deploy.oci_repository. Default production.
	StackTag string `json:"stack_tag" yaml:"stack_tag"`
	CodeTag  string `json:"code_tag" yaml:"code_tag"`
	// SettleSeconds delays a new Magento release on servers that are not the leader, so the leader runs
	// setup:upgrade first. Default 0.
	SettleSeconds int64 `json:"settle_seconds" yaml:"settle_seconds"`
	// CheckPath: POST starts a check now, without authentication, at most every 10 seconds. Default /check,
	// "-" disables it.
	CheckPath string `json:"check_path" yaml:"check_path"`
	// StatusPath: GET returns the deployed versions and the last error as JSON. Default /status, "-" disables it.
	StatusPath string `json:"status_path" yaml:"status_path"`
}

const (
	minWatchInterval = 30 * time.Second
	maxWatchBackoff  = time.Hour
	checkRateLimit   = 10 * time.Second
)

func (c *WatchConfig) applyDefaults() {
	if c.IntervalSeconds == 0 {
		c.IntervalSeconds = 300
	}
	if c.StackTag == "" {
		c.StackTag = "production"
	}
	if c.CodeTag == "" {
		c.CodeTag = "production"
	}
	if c.CheckPath == "" {
		c.CheckPath = "/check"
	}
	if c.StatusPath == "" {
		c.StatusPath = "/status"
	}
}

// validate checks the watch section; cfg is the whole config (sources, keys, webhook path).
func (c WatchConfig) validate(cfg Config) error {
	if !c.Enabled {
		return nil
	}
	if time.Duration(c.IntervalSeconds)*time.Second < minWatchInterval {
		return fmt.Errorf("watch.interval_seconds must be at least %d", int(minWatchInterval.Seconds()))
	}
	if c.SettleSeconds < 0 {
		return errors.New("watch.settle_seconds must not be negative")
	}
	for field, tag := range map[string]string{"watch.stack_tag": c.StackTag, "watch.code_tag": c.CodeTag} {
		if !isSafeName(tag) {
			return fmt.Errorf("%s: invalid tag %q", field, tag)
		}
	}
	if err := codeFromRegistry(cfg.Deploy); err != nil {
		return fmt.Errorf("watch %w", err)
	}
	if cfg.Stack.Enabled {
		if err := stackFromRegistry(cfg.Stack); err != nil {
			return fmt.Errorf("watch %w", err)
		}
	}
	for field, path := range map[string]string{"watch.check_path": c.CheckPath, "watch.status_path": c.StatusPath} {
		if path == "-" {
			continue
		}
		if !strings.HasPrefix(path, "/") || path == cfg.WebhookPath {
			return fmt.Errorf("%s must start with / and differ from webhook_path", field)
		}
	}
	if c.CheckPath == c.StatusPath && c.CheckPath != "-" {
		return errors.New("watch.check_path and watch.status_path must differ")
	}
	return nil
}

// endpoint is a configured path, "" when "-" disables it.
func endpoint(path string) string {
	if path == "-" {
		return ""
	}
	return path
}

// targetStatus is what GET <status_path> reports for the stack or the code.
type targetStatus struct {
	// Tag is the moving tag that is watched, Version the release that runs now.
	Tag     string `json:"tag"`
	Version string `json:"version"`
	// FailedVersion and Error describe the last failed check or deploy; empty after a successful one.
	FailedVersion string    `json:"failed_version,omitempty"`
	Error         string    `json:"error,omitempty"`
	CheckedAt     time.Time `json:"checked_at,omitzero"`
}

// watchTarget is the state of one moving tag; only the watch loop changes it.
type watchTarget struct {
	name       string // stack or code
	repository string
	tag        string
	// applied is the manifest digest last deployed (or found current), failed the one whose deploy failed.
	applied, failed string
	// pending and pendingSince: a new code release waiting for settle_seconds on a server that is not the leader.
	pending      string
	pendingSince time.Time
	// backoff counts registry answers 429 in a row; nextCheck is when the target is checked again.
	backoff   int
	nextCheck time.Time
	status    targetStatus
}

type watcher struct {
	cfg    Config
	server *Server
	oci    ociClient
	nudge  chan struct{}
	now    func() time.Time

	targets []*watchTarget

	mu        sync.Mutex // status of the targets, lastNudge
	lastNudge time.Time
}

func newWatcher(cfg Config, s *Server) *watcher {
	w := &watcher{cfg: cfg, server: s, oci: ociClient{cfg: cfg.OCI}, nudge: make(chan struct{}, 1), now: time.Now}
	if s.stack != nil {
		w.targets = append(w.targets, &watchTarget{name: "stack", repository: cfg.Stack.OCIRepository, tag: cfg.Watch.StackTag})
	}
	// the stack before the code: setup:upgrade of the code needs the running database
	w.targets = append(w.targets, &watchTarget{name: "code", repository: cfg.Deploy.OCIRepository, tag: cfg.Watch.CodeTag})
	for _, t := range w.targets {
		t.status.Tag = t.tag
	}
	return w
}

// run checks the tags at start, then every interval or when nudged, until ctx ends. A running deploy is not
// cancelled with ctx: it finishes, then run returns.
func (w *watcher) run(ctx context.Context) {
	log.Printf("watch: every %ds, stack tag %s, code tag %s", w.cfg.Watch.IntervalSeconds, w.cfg.Watch.StackTag, w.cfg.Watch.CodeTag)
	for {
		w.checkAll(context.WithoutCancel(ctx))
		select {
		case <-ctx.Done():
			return
		case <-time.After(w.wait()):
		case <-w.nudge:
		}
	}
}

func (w *watcher) wait() time.Duration {
	interval := time.Duration(w.cfg.Watch.IntervalSeconds) * time.Second
	return interval + time.Duration(rand.Int64N(int64(interval)/10+1))
}

func (w *watcher) checkAll(ctx context.Context) {
	for _, t := range w.targets {
		w.check(ctx, t)
	}
}

// check deploys what t.tag points to when it changed since the last check.
func (w *watcher) check(ctx context.Context, t *watchTarget) {
	now := w.now()
	if now.Before(t.nextCheck) {
		return
	}
	digest, err := w.oci.head(ctx, t.repository, t.tag)
	if err != nil {
		var terr *transport.Error
		if errors.As(err, &terr) && terr.StatusCode == http.StatusTooManyRequests {
			t.backoff++
			delay := min(time.Duration(w.cfg.Watch.IntervalSeconds)*time.Second<<t.backoff, maxWatchBackoff)
			t.nextCheck = now.Add(delay)
			err = fmt.Errorf("registry rate limit, next check in %s: %w", delay, err)
		}
		w.setStatus(t, now, "", err)
		log.Printf("watch %s: %v", t.name, err)
		return
	}
	t.backoff = 0
	if digest == t.applied || digest == t.failed {
		w.setStatus(t, now, t.status.FailedVersion, errorOrNil(t.status.Error))
		return
	}
	if t.name == "code" && w.cfg.Watch.SettleSeconds > 0 && !leaderFromEnv(w.cfg.Deploy) {
		if t.pending != digest {
			t.pending, t.pendingSince = digest, now
		}
		if settle := time.Duration(w.cfg.Watch.SettleSeconds) * time.Second; now.Sub(t.pendingSince) < settle {
			log.Printf("watch code: %s:%s changed, deploying after %s (not the leader)", t.repository, t.tag, settle-now.Sub(t.pendingSince))
			return
		}
	}

	a, err := resolveRelease(ctx, w.oci, t.repository, t.tag)
	if err != nil {
		w.setStatus(t, now, "", err)
		log.Printf("watch %s: %v", t.name, err)
		return
	}
	unlock, err := w.server.lockDeploys()
	if err == nil {
		if t.name == "stack" {
			err = deployStackRelease(ctx, w.cfg, w.server, a, nil)
		} else {
			err = deployCodeRelease(ctx, w.cfg, w.server, a)
		}
		unlock()
	}
	if err != nil {
		// not again until the tag moves: a broken release would be rolled back on every check
		t.failed = digest
		w.setStatus(t, now, a.Version, err)
		log.Printf("watch %s: %s failed, not retried until %s moves: %v", t.name, a.Version, t.tag, err)
		return
	}
	t.applied, t.failed, t.pending = digest, "", ""
	w.setStatus(t, now, "", nil)
}

func errorOrNil(msg string) error {
	if msg == "" {
		return nil
	}
	return errors.New(msg)
}

func (w *watcher) setStatus(t *watchTarget, now time.Time, failedVersion string, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	t.status.CheckedAt = now
	t.status.FailedVersion = failedVersion
	t.status.Error = ""
	if err != nil {
		t.status.Error = logSafe(err.Error())
	}
}

// serveCheck queues a check: 202, or 429 within checkRateLimit of the last one. No authentication: the worst
// a caller can do is make deputy look at the registry.
func (w *watcher) serveCheck(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		rw.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 4096))
	w.mu.Lock()
	now := w.now()
	limited := !w.lastNudge.IsZero() && now.Sub(w.lastNudge) < checkRateLimit
	if !limited {
		w.lastNudge = now
	}
	w.mu.Unlock()
	if limited {
		http.Error(rw, "check already requested", http.StatusTooManyRequests)
		return
	}
	select {
	case w.nudge <- struct{}{}:
	default: // a check is queued already
	}
	rw.WriteHeader(http.StatusAccepted)
	_, _ = rw.Write([]byte("check queued\n"))
}

// watchStatus is the JSON of GET <status_path>.
type watchStatus struct {
	Deputy string        `json:"deputy"`
	Stack  *targetStatus `json:"stack,omitempty"`
	Code   targetStatus  `json:"code"`
}

func (w *watcher) status() watchStatus {
	st := watchStatus{Deputy: version.Version}
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, t := range w.targets {
		s := t.status
		if t.name == "stack" {
			if d, ok := w.server.stack.(*StackDeployer); ok {
				_, s.Version = d.currentRelease()
			}
			st.Stack = &s
		} else {
			s.Version = currentCodeRelease(w.cfg)
			st.Code = s
		}
	}
	return st
}

func (w *watcher) serveStatus(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		rw.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	rw.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(rw).Encode(w.status())
}
