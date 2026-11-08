package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	awsv2 "github.com/aws/aws-sdk-go-v2/aws"
	sdkconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"gopkg.in/yaml.v3"

	"github.com/magenxcommerce/magenx-deputy-go/version"
)

type Config struct {
	ListenAddr               string `json:"listen_addr" yaml:"listen_addr"`
	WebhookPath              string `json:"webhook_path" yaml:"webhook_path"`
	WebhookSecret            string `json:"webhook_secret" yaml:"webhook_secret"`
	HMACKey                  string `json:"hmac_key" yaml:"hmac_key"`
	TimestampWindowSeconds   int64  `json:"timestamp_window_seconds" yaml:"timestamp_window_seconds"`
	DeploymentTimeoutSeconds int64  `json:"deployment_timeout_seconds" yaml:"deployment_timeout_seconds"`
	// AllowMissingArtifactDigest accepts payloads without artifact_digest. Digests that are sent are always verified.
	AllowMissingArtifactDigest bool           `json:"allow_missing_artifact_digest" yaml:"allow_missing_artifact_digest"`
	Deploy                     DeployConfig   `json:"deploy" yaml:"deploy"`
	HealthCheck                HealthCheck    `json:"health_check" yaml:"health_check"`
	SharedLinks                []SharedTarget `json:"shared_links" yaml:"shared_links"`
	Stack                      StackConfig    `json:"stack" yaml:"stack"`
}

type DeployConfig struct {
	RootDir            string   `json:"root_dir" yaml:"root_dir"`
	ReleasesDirName    string   `json:"releases_dir_name" yaml:"releases_dir_name"`
	CurrentLinkSuffix  string   `json:"current_link_suffix" yaml:"current_link_suffix"`
	DefaultBrand       string   `json:"default_brand" yaml:"default_brand"`
	DefaultDomain      string   `json:"default_domain" yaml:"default_domain"`
	MinimumDiskSpaceGB uint64   `json:"minimum_disk_space_gb" yaml:"minimum_disk_space_gb"`
	S3Bucket           string   `json:"s3_bucket" yaml:"s3_bucket"`
	S3BucketPerBrand   bool     `json:"s3_bucket_per_brand" yaml:"s3_bucket_per_brand"`
	S3Prefix           string   `json:"s3_prefix" yaml:"s3_prefix"`
	S3Endpoint         string   `json:"s3_endpoint" yaml:"s3_endpoint"`
	S3Region           string   `json:"s3_region" yaml:"s3_region"`
	S3UsePathStyle     bool     `json:"s3_use_path_style" yaml:"s3_use_path_style"`
	LeaderEnvVar       string   `json:"leader_env_var" yaml:"leader_env_var"`
	LeaderDefault      bool     `json:"leader_default" yaml:"leader_default"`
	LeaderCommands     []string `json:"leader_commands" yaml:"leader_commands"`
	// PreSwitchCommands run on the leader inside the new release before the current symlink switches.
	PreSwitchCommands []string `json:"pre_switch_commands" yaml:"pre_switch_commands"`
	// RollbackCommands run on the leader after the current symlink was switched back to the previous release.
	RollbackCommands       []string `json:"rollback_commands" yaml:"rollback_commands"`
	RollbackTimeoutSeconds int64    `json:"rollback_timeout_seconds" yaml:"rollback_timeout_seconds"`
	// KeepReleases is the number of release directories kept after a successful deploy. Negative disables pruning.
	KeepReleases int `json:"keep_releases" yaml:"keep_releases"`
	// RunAs is the user ("user", "user:group", "uid", "uid:gid") that owns the releases and runs the commands.
	// Empty is the deputy process user. Another user needs deputy to run as root.
	RunAs string `json:"run_as" yaml:"run_as"`
	// CommandEnv is passed to commands: KEY=VALUE, or NAME to pass deputy's own value of NAME.
	// Commands do not inherit the deputy environment.
	CommandEnv []string `json:"command_env" yaml:"command_env"`
	// AllowRootCommands permits commands to run as root (run_as root, or empty while deputy runs as root).
	AllowRootCommands bool `json:"allow_root_commands" yaml:"allow_root_commands"`
}

type HealthCheck struct {
	Enabled              bool   `json:"enabled" yaml:"enabled"`
	Scheme               string `json:"scheme" yaml:"scheme"`
	PathTemplate         string `json:"path_template" yaml:"path_template"`
	ExpectedStatus       int    `json:"expected_status" yaml:"expected_status"`
	TimeoutSeconds       int64  `json:"timeout_seconds" yaml:"timeout_seconds"`
	UserAgentPrefix      string `json:"user_agent_prefix" yaml:"user_agent_prefix"`
	Attempts             int    `json:"attempts" yaml:"attempts"`
	RetryIntervalSeconds int64  `json:"retry_interval_seconds" yaml:"retry_interval_seconds"`
}

type SharedTarget struct {
	Source string `json:"source" yaml:"source"`
	Target string `json:"target" yaml:"target"`
}

type DeployPayload struct {
	// Kind selects the deployer: "" or "code" for a Magento release from S3, "stack" for a docker stack release.
	Kind           string `json:"kind" yaml:"kind"`
	ArtifactName   string `json:"artifact_name" yaml:"artifact_name"`
	ArtifactDigest string `json:"artifact_digest" yaml:"artifact_digest"`
	Brand          string `json:"brand" yaml:"brand"`
	Domain         string `json:"domain" yaml:"domain"`
	Timestamp      int64  `json:"timestamp" yaml:"timestamp"`
	// Tag is the release of the docker stack (kind stack): name of the release dir, bundle stack_<tag>.tar.gz.
	Tag string `json:"tag" yaml:"tag"`
	// Confirm lists services listed in stack.confirm_services whose image may change with this release.
	Confirm []string `json:"confirm" yaml:"confirm"`
}

type Deployer interface {
	Deploy(context.Context, DeployPayload) error
}

type Server struct {
	cfg      Config
	deployer Deployer
	// stack is nil when stack.enabled is false.
	stack Deployer
	// mu serializes all deploys, code and stack.
	mu sync.Mutex
}

var (
	brandPattern    = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	artifactPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+\.(?:tar\.gz|tgz)$`)
	digestPattern   = regexp.MustCompile(`^sha256:[0-9a-fA-F]{64}$`)
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Printf("magenx-deputy-go %s\n", version.Version)
		os.Exit(0)
	}

	configPath := flag.String("config", "config.yaml", "Path to config YAML file")
	flag.Parse()

	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}
	if cfg.WebhookSecret == "" || cfg.HMACKey == "" {
		log.Fatal("webhook_secret and hmac_key are required")
	}

	codeID, err := resolveIdentity(cfg.Deploy.RunAs)
	if err != nil {
		log.Fatalf("deploy.run_as: %v", err)
	}
	if codeID.isRoot() && !cfg.Deploy.AllowRootCommands {
		log.Fatal("deploy commands would run as root: set deploy.run_as to the site user, or deploy.allow_root_commands: true")
	}
	server := &Server{cfg: cfg, deployer: &S3MagentoDeployer{cfg: cfg, run: newRunner(codeID, cfg.Deploy.CommandEnv)}}
	log.Printf("code deploys run as %s", codeID)

	if cfg.Stack.Enabled {
		stackID, err := resolveIdentity(cfg.Stack.RunAs)
		if err != nil {
			log.Fatalf("stack.run_as: %v", err)
		}
		stack := newStackDeployer(cfg, newRunner(stackID, cfg.Stack.CommandEnv))
		if err := stack.checkTools(); err != nil {
			log.Fatalf("stack: %v", err)
		}
		server.stack = stack
		log.Printf("stack deploys into %s run as %s", cfg.Stack.Dir, stackID)
	}

	srv := &http.Server{
		Addr:    cfg.ListenAddr,
		Handler: server,
	}

	log.Printf("listening on %s%s", cfg.ListenAddr, cfg.WebhookPath)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("server failed: %v", err)
	}
}

func loadConfig(path string) (Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return Config{}, err
	}
	defer f.Close()

	var cfg Config
	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, err
	}
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c *Config) applyDefaults() {
	if c.ListenAddr == "" {
		c.ListenAddr = "0.0.0.0:8082"
	}
	if c.WebhookPath == "" {
		c.WebhookPath = "/deploy"
	}
	if c.TimestampWindowSeconds <= 0 {
		c.TimestampWindowSeconds = 300
	}
	if c.DeploymentTimeoutSeconds <= 0 {
		c.DeploymentTimeoutSeconds = 900
	}
	if c.Deploy.RootDir == "" {
		c.Deploy.RootDir = "/home"
	}
	if c.Deploy.ReleasesDirName == "" {
		c.Deploy.ReleasesDirName = "releases"
	}
	if c.Deploy.CurrentLinkSuffix == "" {
		c.Deploy.CurrentLinkSuffix = "public/current"
	}
	if c.Deploy.MinimumDiskSpaceGB == 0 {
		c.Deploy.MinimumDiskSpaceGB = 10
	}
	if c.Deploy.S3Prefix == "" {
		c.Deploy.S3Prefix = "releases"
	}
	if c.Deploy.S3Region == "" {
		c.Deploy.S3Region = "us-east-1"
	}
	if !c.Deploy.S3UsePathStyle {
		c.Deploy.S3UsePathStyle = true
	}
	if c.Deploy.LeaderEnvVar == "" {
		c.Deploy.LeaderEnvVar = "IS_LEADER"
	}
	if c.Deploy.RollbackTimeoutSeconds <= 0 {
		c.Deploy.RollbackTimeoutSeconds = 300
	}
	if c.Deploy.KeepReleases == 0 {
		c.Deploy.KeepReleases = 5
	}
	if c.HealthCheck.Scheme == "" {
		c.HealthCheck.Scheme = "https"
	}
	if c.HealthCheck.PathTemplate == "" {
		c.HealthCheck.PathTemplate = "/health_check.php?release=%s"
	}
	if c.HealthCheck.ExpectedStatus == 0 {
		c.HealthCheck.ExpectedStatus = http.StatusOK
	}
	if c.HealthCheck.TimeoutSeconds == 0 {
		c.HealthCheck.TimeoutSeconds = 20
	}
	if c.HealthCheck.UserAgentPrefix == "" {
		c.HealthCheck.UserAgentPrefix = "Deployment health check"
	}
	if c.HealthCheck.Attempts <= 0 {
		c.HealthCheck.Attempts = 3
	}
	if c.HealthCheck.RetryIntervalSeconds <= 0 {
		c.HealthCheck.RetryIntervalSeconds = 5
	}
	c.Stack.applyDefaults(c.Deploy.DefaultBrand)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	now := time.Now().Unix()
	if r.URL.Path != s.cfg.WebhookPath {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1024*1024))
	if err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}

	if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Webhook-Secret")), []byte(s.cfg.WebhookSecret)) != 1 {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	signature := r.Header.Get("X-Signature")
	if !validateSignature(s.cfg.HMACKey, body, signature) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	timestamp, err := strconv.ParseInt(r.Header.Get("X-Timestamp"), 10, 64)
	if err != nil {
		http.Error(w, "invalid timestamp", http.StatusBadRequest)
		return
	}
	if exceedsTimestampWindow(now, timestamp, s.cfg.TimestampWindowSeconds) {
		http.Error(w, "expired timestamp", http.StatusForbidden)
		return
	}

	var payload DeployPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if payload.Timestamp != 0 && payload.Timestamp != timestamp {
		http.Error(w, "timestamp mismatch", http.StatusBadRequest)
		return
	}

	deployer := s.deployer
	timeout := time.Duration(s.cfg.DeploymentTimeoutSeconds) * time.Second
	switch payload.Kind {
	case "", "code":
		if msg := s.validateCodePayload(&payload); msg != "" {
			http.Error(w, msg, http.StatusBadRequest)
			return
		}
	case "stack":
		if s.stack == nil {
			http.Error(w, "stack deploys are not enabled", http.StatusBadRequest)
			return
		}
		if msg := s.validateStackPayload(&payload); msg != "" {
			http.Error(w, msg, http.StatusBadRequest)
			return
		}
		deployer = s.stack
		timeout = time.Duration(s.cfg.Stack.DeploymentTimeoutSeconds) * time.Second
	default:
		http.Error(w, "unknown kind, expected code or stack", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Detached from the request: a dropped client connection must not kill bin/magento mid-run.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), timeout)
	defer cancel()

	if err := deployer.Deploy(ctx, payload); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// validateCodePayload fills brand and domain from the defaults and returns an error message, or "".
func (s *Server) validateCodePayload(payload *DeployPayload) string {
	if payload.Tag != "" || len(payload.Confirm) > 0 {
		return "tag and confirm are only valid with kind stack"
	}
	brand, err := requireMatchOrEmpty(payload.Brand, s.cfg.Deploy.DefaultBrand)
	if err != nil {
		return "brand does not match configured default"
	}
	domain, err := requireMatchOrEmpty(payload.Domain, s.cfg.Deploy.DefaultDomain)
	if err != nil {
		return "domain does not match configured default"
	}
	payload.Brand = brand
	payload.Domain = domain
	if payload.ArtifactName == "" || payload.Brand == "" {
		return "artifact_name and brand are required"
	}
	if s.cfg.HealthCheck.Enabled && payload.Domain == "" {
		return "domain is required when health_check is enabled"
	}
	if !isValidPayloadPathFields(payload.Brand, payload.ArtifactName) {
		return "invalid brand or artifact_name format"
	}
	if payload.ArtifactDigest == "" && !s.cfg.AllowMissingArtifactDigest {
		return "artifact_digest is required"
	}
	if payload.ArtifactDigest != "" && !digestPattern.MatchString(payload.ArtifactDigest) {
		return "invalid artifact_digest format, expected sha256:<hex>"
	}
	return ""
}

func validateSignature(key string, payload []byte, signature string) bool {
	algo, hashVal, ok := strings.Cut(signature, "=")
	if !ok || algo != "sha256" || hashVal == "" {
		return false
	}
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write(payload)
	expected := mac.Sum(nil)

	received, err := hex.DecodeString(hashVal)
	if err != nil {
		return false
	}
	return hmac.Equal(expected, received)
}

type S3MagentoDeployer struct {
	cfg Config
	// run starts commands and tar as deploy.run_as; files deputy creates are handed to that user.
	run Runner
	// download fetches the artifact to destination and returns its digest; nil uses S3.
	download func(ctx context.Context, brand, artifact, destination string) (string, error)
}

func (d *S3MagentoDeployer) Deploy(ctx context.Context, payload DeployPayload) (err error) {
	artifactFile := filepath.Base(payload.ArtifactName)
	releaseVersion := normalizeReleaseVersion(artifactFile)
	// absolute: symlink targets must not depend on the service working directory
	deployDir, err := filepath.Abs(filepath.Join(d.cfg.Deploy.RootDir, payload.Brand))
	if err != nil {
		return fmt.Errorf("resolve deploy dir: %w", err)
	}
	releasesDir := filepath.Join(deployDir, d.cfg.Deploy.ReleasesDirName)
	currentLink := filepath.Join(deployDir, filepath.FromSlash(d.cfg.Deploy.CurrentLinkSuffix))

	log.Printf("deploy %s: start (brand=%s)", releaseVersion, payload.Brand)
	defer func() {
		if err != nil {
			log.Printf("deploy %s: failed: %v", releaseVersion, err)
		}
	}()

	if err := d.run.ID.mkdirAll(deployDir, 0o750); err != nil {
		return fmt.Errorf("create deploy dir: %w", err)
	}
	if err := ensureDiskSpace(deployDir, d.cfg.Deploy.MinimumDiskSpaceGB); err != nil {
		return err
	}

	releaseDir, err := d.prepareRelease(ctx, payload, releasesDir, releaseVersion)
	if err != nil {
		return err
	}

	// Pre-switch: the old release keeps serving (e.g. setup:upgrade with deployment/blue_green enabled).
	if d.isLeader() {
		for _, command := range d.cfg.Deploy.PreSwitchCommands {
			if err := d.run.Shell(ctx, command, releaseDir); err != nil {
				_ = os.RemoveAll(releaseDir)
				return fmt.Errorf("pre-switch command failed: %w", err)
			}
		}
	}

	previousRelease, _ := os.Readlink(currentLink)
	if previousRelease != "" && !filepath.IsAbs(previousRelease) {
		previousRelease = filepath.Join(filepath.Dir(currentLink), previousRelease)
	}
	switched := false
	defer func() {
		if err == nil || !switched {
			return
		}
		err = errors.Join(err, d.rollback(previousRelease, currentLink))
	}()

	if err := d.run.ID.symlink(releaseDir, currentLink); err != nil {
		return err
	}
	switched = true
	log.Printf("deploy %s: switched %s -> %s", releaseVersion, currentLink, releaseDir)

	if d.isLeader() {
		for _, command := range d.cfg.Deploy.LeaderCommands {
			if err := d.run.Shell(ctx, command, releaseDir); err != nil {
				return fmt.Errorf("leader command failed: %w", err)
			}
		}
	}

	if d.cfg.HealthCheck.Enabled {
		if err := d.healthCheck(ctx, payload.Domain, releaseVersion); err != nil {
			return err
		}
	}

	log.Printf("deploy %s: success", releaseVersion)
	if err := pruneReleases(releasesDir, d.cfg.Deploy.KeepReleases, releaseDir, previousRelease); err != nil {
		log.Printf("deploy %s: prune releases: %v", releaseVersion, err)
	}
	return nil
}

// prepareRelease downloads, verifies and extracts the artifact into a staging directory, then renames it
// to a release directory that did not exist before. An existing (possibly live) release is never touched.
// On error nothing is left behind.
func (d *S3MagentoDeployer) prepareRelease(ctx context.Context, payload DeployPayload, releasesDir, releaseVersion string) (releaseDir string, err error) {
	stagingDir, err := makeStagingDir(d.run.ID, releasesDir, releaseVersion)
	if err != nil {
		return "", err
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(stagingDir)
			if releaseDir != "" {
				_ = os.RemoveAll(releaseDir)
			}
			releaseDir = ""
		}
	}()

	artifactFile := filepath.Base(payload.ArtifactName)
	artifactPath := filepath.Join(stagingDir, artifactFile)
	download := d.download
	if download == nil {
		download = d.downloadArtifact
	}
	actualDigest, err := download(ctx, payload.Brand, artifactFile, artifactPath)
	if err != nil {
		return "", err
	}
	if err := verifyArtifactDigest(payload.ArtifactDigest, actualDigest); err != nil {
		return "", err
	}
	if err := d.run.ID.own(artifactPath); err != nil {
		return "", err
	}
	if err := extractArchive(ctx, d.run, artifactPath, stagingDir, 0); err != nil {
		return "", err
	}
	if err := os.Remove(artifactPath); err != nil {
		return "", fmt.Errorf("remove downloaded artifact: %w", err)
	}

	target := uniqueReleaseDir(releasesDir, releaseVersion, time.Now())
	if err := os.Rename(stagingDir, target); err != nil {
		return "", fmt.Errorf("move release into place: %w", err)
	}
	releaseDir = target

	if err := linkSharedPaths(d.run.ID, releaseDir, d.cfg.SharedLinks); err != nil {
		return "", err
	}
	return releaseDir, nil
}

// rollback points the current symlink back to the previous release and runs rollback commands on the leader.
func (d *S3MagentoDeployer) rollback(previousRelease, currentLink string) error {
	if previousRelease == "" {
		if err := os.Remove(currentLink); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Printf("rollback: remove %s failed: %v", currentLink, err)
			return fmt.Errorf("rollback: remove current link: %w", err)
		}
		log.Printf("rollback: removed %s (no previous release)", currentLink)
		return nil
	}
	if err := d.run.ID.symlink(previousRelease, currentLink); err != nil {
		log.Printf("rollback: switch %s -> %s failed: %v", currentLink, previousRelease, err)
		return fmt.Errorf("rollback: switch to previous release: %w", err)
	}
	log.Printf("rollback: switched %s -> %s", currentLink, previousRelease)

	if !d.isLeader() || len(d.cfg.Deploy.RollbackCommands) == 0 {
		return nil
	}
	// fresh context: the deploy context may already be expired
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(d.cfg.Deploy.RollbackTimeoutSeconds)*time.Second)
	defer cancel()
	var errs []error
	for _, command := range d.cfg.Deploy.RollbackCommands {
		if err := d.run.Shell(ctx, command, previousRelease); err != nil {
			log.Printf("rollback: command failed: %v", err)
			errs = append(errs, fmt.Errorf("rollback command failed: %w", err))
		}
	}
	return errors.Join(errs...)
}

// uniqueReleaseDir returns releases/<version>, or releases/<version>-<UTC timestamp> when that already exists.
func uniqueReleaseDir(releasesDir, version string, now time.Time) string {
	dir := filepath.Join(releasesDir, version)
	if _, err := os.Lstat(dir); errors.Is(err, os.ErrNotExist) {
		return dir
	}
	return filepath.Join(releasesDir, version+"-"+now.UTC().Format("20060102150405"))
}

// pruneReleases removes the oldest release directories beyond keep, plus leftover staging directories.
// Protected paths (new and previous release) are never removed. keep < 0 disables pruning.
func pruneReleases(releasesDir string, keep int, protected ...string) error {
	if keep < 0 {
		return nil
	}
	entries, err := os.ReadDir(releasesDir)
	if err != nil {
		return err
	}
	isProtected := func(path string) bool {
		for _, p := range protected {
			if p != "" && filepath.Clean(p) == filepath.Clean(path) {
				return true
			}
		}
		return false
	}

	type release struct {
		path    string
		modTime time.Time
	}
	var releases []release
	var errs []error
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(releasesDir, entry.Name())
		if strings.HasPrefix(entry.Name(), ".") {
			if strings.Contains(entry.Name(), ".tmp-") {
				errs = append(errs, os.RemoveAll(path))
			}
			continue
		}
		info, err := entry.Info()
		if err != nil {
			errs = append(errs, err)
			continue
		}
		releases = append(releases, release{path: path, modTime: info.ModTime()})
	}
	sort.Slice(releases, func(i, j int) bool { return releases[i].modTime.After(releases[j].modTime) })

	kept := 0
	for _, r := range releases {
		if isProtected(r.path) || kept < keep {
			kept++
			continue
		}
		log.Printf("prune: removing old release %s", r.path)
		errs = append(errs, os.RemoveAll(r.path))
	}
	return errors.Join(errs...)
}

func (d *S3MagentoDeployer) isLeader() bool {
	value := strings.ToLower(strings.TrimSpace(os.Getenv(d.cfg.Deploy.LeaderEnvVar)))
	if value == "" {
		return d.cfg.Deploy.LeaderDefault
	}
	return value == "1" || value == "true" || value == "yes"
}

// healthCheck retries up to HealthCheck.Attempts times, waiting RetryIntervalSeconds between attempts.
func (d *S3MagentoDeployer) healthCheck(ctx context.Context, domain, releaseVersion string) error {
	if domain == "" {
		return errors.New("domain is required for health check")
	}
	var err error
	for attempt := 1; attempt <= d.cfg.HealthCheck.Attempts; attempt++ {
		if err = d.healthCheckOnce(ctx, domain, releaseVersion); err == nil {
			return nil
		}
		log.Printf("deploy %s: health check attempt %d/%d: %v", releaseVersion, attempt, d.cfg.HealthCheck.Attempts, err)
		if attempt == d.cfg.HealthCheck.Attempts {
			break
		}
		select {
		case <-ctx.Done():
			return errors.Join(err, ctx.Err())
		case <-time.After(time.Duration(d.cfg.HealthCheck.RetryIntervalSeconds) * time.Second):
		}
	}
	return err
}

func (d *S3MagentoDeployer) healthCheckOnce(ctx context.Context, domain, releaseVersion string) error {
	path := fmt.Sprintf(d.cfg.HealthCheck.PathTemplate, url.QueryEscape(releaseVersion))
	checkURL := fmt.Sprintf("%s://%s%s", d.cfg.HealthCheck.Scheme, domain, path)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, checkURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", fmt.Sprintf("%s for %s", d.cfg.HealthCheck.UserAgentPrefix, releaseVersion))

	client := &http.Client{Timeout: time.Duration(d.cfg.HealthCheck.TimeoutSeconds) * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("health check failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != d.cfg.HealthCheck.ExpectedStatus {
		return fmt.Errorf("health check returned %d", resp.StatusCode)
	}
	return nil
}

// downloadArtifact writes the S3 object to destination and returns its sha256 as "sha256:<hex>".
func (d *S3MagentoDeployer) downloadArtifact(ctx context.Context, brand, artifact, destination string) (string, error) {
	return downloadS3(ctx, d.cfg.Deploy, brand, d.cfg.Deploy.S3Prefix, artifact, destination)
}

// downloadS3 writes s3://<bucket>/<prefix>/<artifact> to destination and returns its sha256 as "sha256:<hex>".
// Bucket, endpoint, region and path style come from the deploy section; the bucket is the brand with
// s3_bucket_per_brand.
func downloadS3(ctx context.Context, dc DeployConfig, brand, prefix, artifact, destination string) (string, error) {
	bucket := dc.S3Bucket
	if dc.S3BucketPerBrand {
		bucket = brand
	}
	if bucket == "" {
		return "", errors.New("s3 bucket is not configured")
	}

	prefix = strings.Trim(prefix, "/")
	artifactPath := artifact
	if prefix != "" {
		artifactPath = prefix + "/" + artifact
	}
	s3URI := fmt.Sprintf("s3://%s/%s", bucket, artifactPath)

	loadOptions := []func(*sdkconfig.LoadOptions) error{
		sdkconfig.WithRegion(dc.S3Region),
	}
	if dc.S3Endpoint != "" {
		resolver := awsv2.EndpointResolverWithOptionsFunc(func(service, region string, options ...interface{}) (awsv2.Endpoint, error) {
			if service == s3.ServiceID {
				return awsv2.Endpoint{
					URL:               dc.S3Endpoint,
					HostnameImmutable: true,
				}, nil
			}
			return awsv2.Endpoint{}, &awsv2.EndpointNotFoundError{}
		})
		loadOptions = append(loadOptions, sdkconfig.WithEndpointResolverWithOptions(resolver))
	}

	awsCfg, err := sdkconfig.LoadDefaultConfig(ctx, loadOptions...)
	if err != nil {
		return "", fmt.Errorf("load aws config: %w", err)
	}

	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.UsePathStyle = dc.S3UsePathStyle
	})

	obj, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: awsv2.String(bucket),
		Key:    awsv2.String(artifactPath),
	})
	if err != nil {
		return "", fmt.Errorf("download artifact %s: %w", s3URI, err)
	}
	defer obj.Body.Close()

	out, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
	if err != nil {
		return "", fmt.Errorf("create local artifact file: %w", err)
	}
	digest, err := copyWithSHA256(out, obj.Body)
	if closeErr := out.Close(); err == nil && closeErr != nil {
		err = closeErr
	}
	if err != nil {
		return "", fmt.Errorf("write downloaded artifact: %w", err)
	}
	return digest, nil
}

// copyWithSHA256 copies src to dst and returns the sha256 of the copied bytes as "sha256:<hex>".
func copyWithSHA256(dst io.Writer, src io.Reader) (string, error) {
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(dst, h), src); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// verifyArtifactDigest compares the expected payload digest with the downloaded one.
// An empty expected digest is accepted; the webhook handler decides whether it is allowed.
func verifyArtifactDigest(expected, actual string) error {
	if expected == "" {
		return nil
	}
	if !strings.EqualFold(expected, actual) {
		return fmt.Errorf("artifact digest mismatch: expected %s, got %s", strings.ToLower(expected), actual)
	}
	return nil
}

func ensureDiskSpace(path string, requiredGB uint64) error {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return fmt.Errorf("read filesystem stats: %w", err)
	}
	available := uint64(stat.Bavail) * uint64(stat.Bsize)
	required := requiredGB * 1024 * 1024 * 1024
	if available < required {
		return fmt.Errorf("not enough disk space: required=%dGB available=%dGB", requiredGB, available/(1024*1024*1024))
	}
	return nil
}

// makeStagingDir creates <releasesDir>/.<version>.tmp-* owned by id.
func makeStagingDir(id Identity, releasesDir, version string) (string, error) {
	if err := id.mkdirAll(releasesDir, 0o750); err != nil {
		return "", fmt.Errorf("create releases dir: %w", err)
	}
	stagingDir, err := os.MkdirTemp(releasesDir, "."+version+".tmp-")
	if err != nil {
		return "", fmt.Errorf("create staging dir: %w", err)
	}
	if err := os.Chmod(stagingDir, 0o750); err != nil {
		_ = os.RemoveAll(stagingDir)
		return "", fmt.Errorf("chmod staging dir: %w", err)
	}
	if err := id.own(stagingDir); err != nil {
		_ = os.RemoveAll(stagingDir)
		return "", err
	}
	return stagingDir, nil
}

// linkSharedPaths replaces each target inside the release with a symlink to its source.
// Directories on the way are resolved inside the release only: a release symlink that leads elsewhere
// is refused, so deputy never creates files outside the release.
func linkSharedPaths(id Identity, releaseDir string, links []SharedTarget) error {
	for _, shared := range links {
		if shared.Source == "" || shared.Target == "" {
			return errors.New("shared link source and target are required")
		}
		targetPath, err := safeJoin(releaseDir, filepath.FromSlash(strings.TrimPrefix(shared.Target, "/")))
		if err != nil {
			return err
		}
		if err := requireInside(releaseDir, filepath.Dir(targetPath)); err != nil {
			return fmt.Errorf("shared link %s: %w", shared.Target, err)
		}
		if err := id.mkdirAll(filepath.Dir(targetPath), 0o750); err != nil {
			return err
		}
		if err := os.RemoveAll(targetPath); err != nil {
			return err
		}
		if err := os.Symlink(shared.Source, targetPath); err != nil {
			return err
		}
		if err := id.own(targetPath); err != nil {
			return err
		}
	}
	return nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func normalizeReleaseVersion(artifactName string) string {
	name := filepath.Base(artifactName)
	name = strings.TrimPrefix(name, "release_")
	name = strings.TrimSuffix(name, ".tar.gz")
	name = strings.TrimSuffix(name, ".tgz")
	if name == "" {
		return "unknown-release"
	}
	return name
}

func exceedsTimestampWindow(now, timestamp, window int64) bool {
	if now >= timestamp {
		return now-timestamp > window
	}
	return timestamp-now > window
}

// requireInside fails when dir, with symlinks resolved as far as it exists, is not inside base.
func requireInside(base, dir string) error {
	realBase, err := filepath.EvalSymlinks(base)
	if err != nil {
		return err
	}
	existing := dir
	for {
		if _, err := os.Lstat(existing); err == nil {
			break
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			break
		}
		existing = parent
	}
	real, err := filepath.EvalSymlinks(existing)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(realBase, real)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return fmt.Errorf("%s resolves outside the release", dir)
	}
	return nil
}

func safeJoin(base, unsafePath string) (string, error) {
	target := filepath.Join(base, unsafePath)
	rel, err := filepath.Rel(base, target)
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", errors.New("path escapes base directory")
	}
	return target, nil
}

func isValidPayloadPathFields(brand, artifactName string) bool {
	if !brandPattern.MatchString(brand) {
		return false
	}
	if filepath.Base(artifactName) != artifactName {
		return false
	}
	return artifactPattern.MatchString(artifactName)
}

func requireMatchOrEmpty(requested, configured string) (string, error) {
	if requested != "" && requested != configured {
		return "", errors.New("override value does not match configured value")
	}
	return configured, nil
}

func (c Config) validate() error {
	if c.Deploy.DefaultBrand == "" || !brandPattern.MatchString(c.Deploy.DefaultBrand) {
		return errors.New("deploy.default_brand is required and must match [A-Za-z0-9._-]+")
	}
	if c.Deploy.DefaultDomain == "" {
		return errors.New("deploy.default_domain is required")
	}
	if c.Deploy.S3Endpoint != "" {
		u, err := url.Parse(c.Deploy.S3Endpoint)
		if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
			return errors.New("deploy.s3_endpoint must be a valid http/https URL")
		}
	}
	if c.Stack.Enabled && !c.Deploy.S3BucketPerBrand && c.Deploy.S3Bucket == "" {
		return errors.New("stack deploys download from S3: set deploy.s3_bucket or deploy.s3_bucket_per_brand")
	}
	return c.Stack.validate()
}
