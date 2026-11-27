package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/magenxcommerce/magenx-deputy-go/version"
)

// Config: deputy deploys the releases that moving tags in an OCI registry point to (watch, bootstrap). There
// is no inbound deploy request: the cosign signatures are the trust anchor.
type Config struct {
	// ListenAddr serves POST watch.check_path and GET watch.status_path.
	ListenAddr               string         `json:"listen_addr" yaml:"listen_addr"`
	DeploymentTimeoutSeconds int64          `json:"deployment_timeout_seconds" yaml:"deployment_timeout_seconds"`
	Deploy                   DeployConfig   `json:"deploy" yaml:"deploy"`
	HealthCheck              HealthCheck    `json:"health_check" yaml:"health_check"`
	SharedLinks              []SharedTarget `json:"shared_links" yaml:"shared_links"`
	Stack                    StackConfig    `json:"stack" yaml:"stack"`
	// OCI is the registry login for the archives (code and stack).
	OCI OCIConfig `json:"oci" yaml:"oci"`
	// Watch deploys what the moving tags point to.
	Watch WatchConfig `json:"watch" yaml:"watch"`
}

type DeployConfig struct {
	RootDir            string `json:"root_dir" yaml:"root_dir"`
	ReleasesDirName    string `json:"releases_dir_name" yaml:"releases_dir_name"`
	CurrentLinkSuffix  string `json:"current_link_suffix" yaml:"current_link_suffix"`
	DefaultBrand       string `json:"default_brand" yaml:"default_brand"`
	DefaultDomain      string `json:"default_domain" yaml:"default_domain"`
	MinimumDiskSpaceGB uint64 `json:"minimum_disk_space_gb" yaml:"minimum_disk_space_gb"`
	// OCIRepository holds one image per release (tag = version) whose only layer is the archive.
	OCIRepository string `json:"oci_repository" yaml:"oci_repository"`
	// CosignPublicKey verifies the signature of the release manifest before the archive is downloaded.
	CosignPublicKey string   `json:"cosign_public_key" yaml:"cosign_public_key"`
	CosignBinary    string   `json:"cosign_binary" yaml:"cosign_binary"`
	CosignArgs      []string `json:"cosign_args" yaml:"cosign_args"`
	LeaderEnvVar    string   `json:"leader_env_var" yaml:"leader_env_var"`
	LeaderDefault   bool     `json:"leader_default" yaml:"leader_default"`
	LeaderCommands  []string `json:"leader_commands" yaml:"leader_commands"`
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
	// CommandsRunAs runs pre_switch_commands, leader_commands and rollback_commands as another user than
	// run_as, which still owns the release files, e.g. root for commands that need docker. Empty is run_as.
	CommandsRunAs string `json:"commands_run_as" yaml:"commands_run_as"`
	// CommandEnv is passed to commands: KEY=VALUE, or NAME to pass deputy's own value of NAME.
	// Commands do not inherit the deputy environment.
	CommandEnv []string `json:"command_env" yaml:"command_env"`
	// AllowRootCommands permits commands to run as root (commands_run_as or run_as root, or empty while deputy
	// runs as root).
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

// Release is one release to deploy, resolved from a moving tag (watch, bootstrap).
type Release struct {
	// Tag is the image tag of the release (its version) and the name of its release dir.
	Tag string
	// ArtifactDigest is the archive, the only layer of the image: sha256:<hex>.
	ArtifactDigest string
	// Confirm, stack deploys only: services of stack.confirm_services whose image may change.
	Confirm []string
}

type Deployer interface {
	Deploy(context.Context, Release) error
}

type Server struct {
	cfg      Config
	deployer Deployer
	// stack is nil when stack.enabled is false.
	stack Deployer
	// mu serializes all deploys, code and stack; lockDeploys takes it.
	mu sync.Mutex
	// watch is nil in bootstrap, which runs no watch.
	watch *watcher
	// lockPath is the file lockDeploys flocks across processes: the config file; empty takes only mu.
	lockPath string
}

var (
	brandPattern  = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	digestPattern = regexp.MustCompile(`^sha256:[0-9a-fA-F]{64}$`)
	// namePattern is one path element: release versions and tags. Deployers check it, plus "no ..",
	// where they build paths.
	namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,127}$`)
)

// logSafe removes CR and LF, so a value from the registry (the version annotation) or the command line can
// never start a new log line. Release values pass through it where a deployer takes them over.
func logSafe(s string) string {
	s = strings.ReplaceAll(s, "\n", "")
	return strings.ReplaceAll(s, "\r", "")
}

// isSafeName reports whether s can be used as one path element: namePattern and no "..".
func isSafeName(s string) bool {
	return namePattern.MatchString(s) && !strings.Contains(s, "..")
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Printf("magenx-deputy-go %s\n", version.Version)
		os.Exit(0)
	}
	if len(os.Args) > 1 && os.Args[1] == "bootstrap" {
		os.Exit(runBootstrap(os.Args[2:]))
	}

	configPath := flag.String("config", "config.yaml", "Path to config YAML file")
	flag.Parse()

	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}
	server, err := buildServer(cfg)
	if err != nil {
		log.Fatal(err)
	}
	server.lockPath = *configPath
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	server.watch = newWatcher(cfg, server)
	if !cfg.listens() {
		// no inbound port at all: the registry is only polled
		server.watch.run(ctx)
		return
	}
	go server.watch.run(ctx)

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           server,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	log.Printf("watch endpoints on %s: check %q, status %q", cfg.ListenAddr, endpoint(cfg.Watch.CheckPath), endpoint(cfg.Watch.StatusPath))
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("server failed: %v", err)
	}
}

// buildServer creates the code deployer and, with stack.enabled, the stack deployer.
func buildServer(cfg Config) (*Server, error) {
	codeID, err := resolveIdentity(cfg.Deploy.RunAs)
	if err != nil {
		return nil, fmt.Errorf("deploy.run_as: %w", err)
	}
	cmdID := codeID
	if cfg.Deploy.CommandsRunAs != "" {
		if cmdID, err = resolveIdentity(cfg.Deploy.CommandsRunAs); err != nil {
			return nil, fmt.Errorf("deploy.commands_run_as: %w", err)
		}
	}
	if cmdID.isRoot() && !cfg.Deploy.AllowRootCommands {
		return nil, errors.New("deploy commands would run as root: set deploy.run_as (or deploy.commands_run_as) to the site user, or deploy.allow_root_commands: true")
	}
	// signature checks run as deputy itself: the registry login never reaches the site user
	ownID, err := currentIdentity()
	if err != nil {
		return nil, err
	}
	oci := ociClient{cfg: cfg.OCI}
	code := &CodeDeployer{cfg: cfg, run: newRunner(codeID, cfg.Deploy.CommandEnv), sigRun: newRunner(ownID, nil), oci: oci}
	if cfg.Deploy.CommandsRunAs != "" {
		cmd := newRunner(cmdID, cfg.Deploy.CommandEnv)
		code.cmd = &cmd
	}
	if err := code.checkTools(); err != nil {
		return nil, fmt.Errorf("deploy: %w", err)
	}
	server := &Server{cfg: cfg, deployer: code}
	log.Printf("code deploys from %s owned by %s, commands run as %s", cfg.Deploy.OCIRepository, codeID, cmdID)

	if cfg.Stack.Enabled {
		stackID, err := resolveIdentity(cfg.Stack.RunAs)
		if err != nil {
			return nil, fmt.Errorf("stack.run_as: %w", err)
		}
		stack := newStackDeployer(cfg, newRunner(stackID, cfg.Stack.CommandEnv))
		stack.sigRun = newRunner(ownID, nil)
		if err := stack.checkTools(); err != nil {
			return nil, fmt.Errorf("stack: %w", err)
		}
		server.stack = stack
		log.Printf("stack deploys from %s into %s run as %s", cfg.Stack.OCIRepository, cfg.Stack.Dir, stackID)
	}
	return server, nil
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
	if c.Deploy.CosignBinary == "" {
		c.Deploy.CosignBinary = "cosign"
	}
	if c.Deploy.CosignArgs == nil {
		// the release workflows sign without transparency log entry
		c.Deploy.CosignArgs = []string{"--insecure-ignore-tlog=true"}
	}
	if c.OCI.LoginEnv == "" {
		c.OCI.LoginEnv = "REGISTRY_PASSWORD"
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
	c.Watch.applyDefaults()
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch path := r.URL.Path; {
	case s.watch != nil && path == endpoint(s.cfg.Watch.CheckPath):
		s.watch.serveCheck(w, r)
	case s.watch != nil && path == endpoint(s.cfg.Watch.StatusPath):
		s.watch.serveStatus(w, r)
	default:
		http.NotFound(w, r)
	}
}

type CodeDeployer struct {
	cfg Config
	// run starts commands and tar as deploy.run_as; files deputy creates are handed to that user.
	run Runner
	// cmd runs the commands instead of run when deploy.commands_run_as is set.
	cmd *Runner
	// download replaces the registry download in tests: writes the archive of tag to destination and returns
	// its digest, without the signature check.
	download func(ctx context.Context, tag, destination string) (string, error)
	// oci pulls the archives; sigRun runs cosign as deputy's own user.
	oci    ociClient
	sigRun Runner
}

// commands is the runner of pre_switch_commands, leader_commands and rollback_commands.
func (d *CodeDeployer) commands() Runner {
	if d.cmd != nil {
		return *d.cmd
	}
	return d.run
}

// codeArtifactName is the file name of a downloaded code release; the release dir is the tag.
func codeArtifactName(tag string) string { return "release_" + tag + ".tar.gz" }

// checkTools fails early when cosign or the public key for code signatures are missing.
func (d *CodeDeployer) checkTools() error {
	if _, err := exec.LookPath(d.cfg.Deploy.CosignBinary); err != nil {
		return fmt.Errorf("%s not found: %w", d.cfg.Deploy.CosignBinary, err)
	}
	if _, err := os.Stat(d.cfg.Deploy.CosignPublicKey); err != nil {
		return fmt.Errorf("cosign public key: %w", err)
	}
	return nil
}

// verifySignature checks the cosign signature of a release manifest with deploy.cosign_public_key.
func (d *CodeDeployer) verifySignature(ctx context.Context, a ociArtifact) error {
	return d.oci.cosignVerifyManifest(ctx, d.sigRun, d.cfg.Deploy.CosignBinary, d.cfg.Deploy.CosignPublicKey, d.cfg.Deploy.CosignArgs, a)
}

func (d *CodeDeployer) Deploy(ctx context.Context, rel Release) (err error) {
	// Paths come from the config and from the tag checked here.
	brand := d.cfg.Deploy.DefaultBrand
	releaseVersion := logSafe(rel.Tag)
	if !isSafeName(releaseVersion) {
		return fmt.Errorf("invalid tag %q", releaseVersion)
	}
	artifactDigest := logSafe(rel.ArtifactDigest)
	if !digestPattern.MatchString(artifactDigest) {
		return fmt.Errorf("invalid artifact digest %q, expected sha256:<hex>", artifactDigest)
	}
	if len(rel.Confirm) > 0 {
		return errors.New("confirm is only valid for stack deploys")
	}
	artifactFile := codeArtifactName(releaseVersion)
	// absolute: symlink targets must not depend on the service working directory
	deployDir, err := filepath.Abs(filepath.Join(d.cfg.Deploy.RootDir, brand))
	if err != nil {
		return fmt.Errorf("resolve deploy dir: %w", err)
	}
	releasesDir := filepath.Join(deployDir, d.cfg.Deploy.ReleasesDirName)
	currentLink := filepath.Join(deployDir, filepath.FromSlash(d.cfg.Deploy.CurrentLinkSuffix))

	log.Printf("deploy %s: start (brand=%s)", releaseVersion, brand)
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

	releaseDir, err := d.prepareRelease(ctx, artifactFile, artifactDigest, releasesDir, releaseVersion)
	if err != nil {
		return err
	}

	// Pre-switch: the old release keeps serving (e.g. setup:upgrade with deployment/blue_green enabled).
	if d.isLeader() {
		for _, command := range d.cfg.Deploy.PreSwitchCommands {
			if err := d.commands().Shell(ctx, command, releaseDir); err != nil {
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
			if err := d.commands().Shell(ctx, command, releaseDir); err != nil {
				return fmt.Errorf("leader command failed: %w", err)
			}
		}
	}

	if d.cfg.HealthCheck.Enabled {
		if err := d.healthCheck(ctx, releaseVersion); err != nil {
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
func (d *CodeDeployer) prepareRelease(ctx context.Context, artifactFile, artifactDigest, releasesDir, releaseVersion string) (releaseDir string, err error) {
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

	artifactPath := filepath.Join(stagingDir, artifactFile)
	download := d.download
	if download == nil {
		// the manifest signature is checked before the download, the layer against artifactDigest
		download = func(ctx context.Context, tag, destination string) (string, error) {
			return d.oci.fetch(ctx, d.cfg.Deploy.OCIRepository, tag, artifactDigest, destination, d.verifySignature)
		}
	}
	actualDigest, err := download(ctx, releaseVersion, artifactPath)
	if err != nil {
		return "", err
	}
	if err := verifyArtifactDigest(artifactDigest, actualDigest); err != nil {
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
	if err := writeReleaseFile(d.run.ID, releaseDir, releaseVersion); err != nil {
		return "", err
	}

	if err := linkSharedPaths(d.run.ID, releaseDir, d.cfg.SharedLinks); err != nil {
		return "", err
	}
	return releaseDir, nil
}

// rollback points the current symlink back to the previous release and runs rollback commands on the leader.
func (d *CodeDeployer) rollback(previousRelease, currentLink string) error {
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
		if err := d.commands().Shell(ctx, command, previousRelease); err != nil {
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

func (d *CodeDeployer) isLeader() bool { return leaderFromEnv(d.cfg.Deploy) }

// leaderFromEnv reports whether this server is the leader: the variable leader_env_var, else leader_default.
func leaderFromEnv(dc DeployConfig) bool {
	value := strings.ToLower(strings.TrimSpace(os.Getenv(dc.LeaderEnvVar)))
	if value == "" {
		return dc.LeaderDefault
	}
	return value == "1" || value == "true" || value == "yes"
}

// healthCheck retries up to HealthCheck.Attempts times, waiting RetryIntervalSeconds between attempts.
func (d *CodeDeployer) healthCheck(ctx context.Context, releaseVersion string) error {
	if d.cfg.Deploy.DefaultDomain == "" {
		return errors.New("deploy.default_domain is required for health check")
	}
	var err error
	for attempt := 1; attempt <= d.cfg.HealthCheck.Attempts; attempt++ {
		if err = d.healthCheckOnce(ctx, releaseVersion); err == nil {
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

// healthCheckURL is <scheme>://<deploy.default_domain>/<path_template with the escaped release version>.
// The host comes from the config only; the release version can only land in the path or query.
func (d *CodeDeployer) healthCheckURL(releaseVersion string) string {
	path := fmt.Sprintf(d.cfg.HealthCheck.PathTemplate, url.QueryEscape(releaseVersion))
	return fmt.Sprintf("%s://%s/%s", d.cfg.HealthCheck.Scheme, d.cfg.Deploy.DefaultDomain, strings.TrimPrefix(path, "/"))
}

func (d *CodeDeployer) healthCheckOnce(ctx context.Context, releaseVersion string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.healthCheckURL(releaseVersion), nil)
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

// copyWithSHA256 copies src to dst and returns the sha256 of the copied bytes as "sha256:<hex>".
func copyWithSHA256(dst io.Writer, src io.Reader) (string, error) {
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(dst, h), src); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// verifyArtifactDigest compares the expected digest (the layer digest of the release) with the downloaded one.
func verifyArtifactDigest(expected, actual string) error {
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

func (c Config) validate() error {
	if c.Deploy.DefaultBrand == "" || !brandPattern.MatchString(c.Deploy.DefaultBrand) {
		return errors.New("deploy.default_brand is required and must match [A-Za-z0-9._-]+")
	}
	if c.Deploy.DefaultDomain == "" {
		return errors.New("deploy.default_domain is required")
	}
	if err := validateOCIRepository("deploy.oci_repository", c.Deploy.OCIRepository); err != nil {
		return err
	}
	if c.Deploy.CosignPublicKey == "" {
		return errors.New("deploy.cosign_public_key is required: releases are only deployed when their signature verifies")
	}
	if err := c.Stack.validate(); err != nil {
		return err
	}
	return c.Watch.validate()
}

// listens: the check or status endpoint of the watch needs listen_addr.
func (c Config) listens() bool {
	return endpoint(c.Watch.CheckPath) != "" || endpoint(c.Watch.StatusPath) != ""
}
