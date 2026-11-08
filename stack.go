package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// StackConfig deploys a docker compose stack from GitHub releases: the repository source at the release
// tag plus the release asset that pins every image by digest (docker-compose.release.yml).
type StackConfig struct {
	Enabled bool `json:"enabled" yaml:"enabled"`
	// Repo is owner/name of the docker configuration repository.
	Repo   string `json:"repo" yaml:"repo"`
	APIURL string `json:"api_url" yaml:"api_url"`
	// TokenEnv names the environment variable with the GitHub token (from /etc/magenx-deputy-go/env).
	// An empty variable means anonymous requests (public repository).
	TokenEnv string `json:"token_env" yaml:"token_env"`
	// RegistryUsername enables "docker login <Registry>" with the GitHub token before pulling.
	// Leave empty when the run_as user is already logged in.
	Registry         string `json:"registry" yaml:"registry"`
	RegistryUsername string `json:"registry_username" yaml:"registry_username"`
	// Dir holds releases/<tag> and the current symlink.
	Dir         string `json:"dir" yaml:"dir"`
	ProjectName string `json:"project_name" yaml:"project_name"`
	// ComposeFiles are relative to the release; LockAsset is downloaded from the release and added last.
	ComposeFiles []string       `json:"compose_files" yaml:"compose_files"`
	LockAsset    string         `json:"lock_asset" yaml:"lock_asset"`
	EnvFiles     []string       `json:"env_files" yaml:"env_files"`
	SharedLinks  []SharedTarget `json:"shared_links" yaml:"shared_links"`
	// CosignPublicKey is the trust anchor. It is never taken from the downloaded release.
	CosignPublicKey string   `json:"cosign_public_key" yaml:"cosign_public_key"`
	CosignArgs      []string `json:"cosign_args" yaml:"cosign_args"`
	RequireSBOM     bool     `json:"require_sbom" yaml:"require_sbom"`
	AllowPrerelease bool     `json:"allow_prerelease" yaml:"allow_prerelease"`
	// RequireImmutable refuses releases that GitHub does not report as immutable.
	RequireImmutable bool   `json:"require_immutable" yaml:"require_immutable"`
	DockerBinary     string `json:"docker_binary" yaml:"docker_binary"`
	CosignBinary     string `json:"cosign_binary" yaml:"cosign_binary"`
	// Services to pull and start; empty is every service of the compose project minus ExcludeServices.
	Services        []string `json:"services" yaml:"services"`
	ExcludeServices []string `json:"exclude_services" yaml:"exclude_services"`
	Profiles        []string `json:"profiles" yaml:"profiles"`
	RemoveOrphans   bool     `json:"remove_orphans" yaml:"remove_orphans"`
	// ConfirmServices only change image when the webhook lists them in "confirm" (e.g. mariadb, opensearch:
	// a major upgrade rewrites their data and cannot be rolled back by switching images).
	ConfirmServices []string `json:"confirm_services" yaml:"confirm_services"`
	// Commands run with /bin/sh in a release dir: pre_up in the new release before the switch,
	// post_up in the new release after "up", rollback in the previous release after switching back.
	PreUpCommands    []string `json:"pre_up_commands" yaml:"pre_up_commands"`
	PostUpCommands   []string `json:"post_up_commands" yaml:"post_up_commands"`
	RollbackCommands []string `json:"rollback_commands" yaml:"rollback_commands"`
	// HealthCheckURL is checked after "up" with the attempts, timeout and status of health_check.
	HealthCheckURL           string `json:"health_check_url" yaml:"health_check_url"`
	WaitTimeoutSeconds       int64  `json:"wait_timeout_seconds" yaml:"wait_timeout_seconds"`
	DeploymentTimeoutSeconds int64  `json:"deployment_timeout_seconds" yaml:"deployment_timeout_seconds"`
	RollbackTimeoutSeconds   int64  `json:"rollback_timeout_seconds" yaml:"rollback_timeout_seconds"`
	MinimumDiskSpaceGB       uint64 `json:"minimum_disk_space_gb" yaml:"minimum_disk_space_gb"`
	KeepReleases             int    `json:"keep_releases" yaml:"keep_releases"`
	// PollIntervalSeconds > 0 also deploys the latest release (no draft, no prerelease) on a timer.
	PollIntervalSeconds int64 `json:"poll_interval_seconds" yaml:"poll_interval_seconds"`
	// RunAs runs docker, cosign, tar and the commands; empty is the deputy process user.
	// The user needs access to the docker socket (root or group docker).
	RunAs      string   `json:"run_as" yaml:"run_as"`
	CommandEnv []string `json:"command_env" yaml:"command_env"`
}

var (
	repoPattern    = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	tagPattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,127}$`)
	servicePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)
	projectPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
	pinnedPattern  = regexp.MustCompile(`@sha256:[0-9a-f]{64}$`)
)

// stackTagFile in a release dir records the tag it was deployed from.
const stackTagFile = ".deputy-release"

func (c *StackConfig) applyDefaults(brand string) {
	if c.APIURL == "" {
		c.APIURL = "https://api.github.com"
	}
	if c.TokenEnv == "" {
		c.TokenEnv = "GITHUB_TOKEN"
	}
	if c.Registry == "" {
		c.Registry = "ghcr.io"
	}
	if c.Dir == "" && brand != "" {
		c.Dir = filepath.Join("/opt", brand, "stack")
	}
	if c.ProjectName == "" {
		c.ProjectName = strings.ToLower(brand)
	}
	if len(c.ComposeFiles) == 0 {
		c.ComposeFiles = []string{"docker-compose.yml"}
	}
	if c.LockAsset == "" {
		c.LockAsset = "docker-compose.release.yml"
	}
	if len(c.EnvFiles) == 0 {
		c.EnvFiles = []string{".env", ".env.secrets"}
	}
	if c.CosignArgs == nil {
		// the release workflow signs without transparency log entry (cosign/signing-config.json)
		c.CosignArgs = []string{"--insecure-ignore-tlog=true"}
	}
	if c.DockerBinary == "" {
		c.DockerBinary = "docker"
	}
	if c.CosignBinary == "" {
		c.CosignBinary = "cosign"
	}
	if c.WaitTimeoutSeconds <= 0 {
		c.WaitTimeoutSeconds = 600
	}
	if c.DeploymentTimeoutSeconds <= 0 {
		c.DeploymentTimeoutSeconds = 1800
	}
	if c.RollbackTimeoutSeconds <= 0 {
		c.RollbackTimeoutSeconds = 900
	}
	if c.MinimumDiskSpaceGB == 0 {
		c.MinimumDiskSpaceGB = 5
	}
	if c.KeepReleases == 0 {
		c.KeepReleases = 3
	}
}

func (c StackConfig) validate() error {
	if !c.Enabled {
		return nil
	}
	if !repoPattern.MatchString(c.Repo) {
		return errors.New("stack.repo is required as owner/name")
	}
	if u, err := url.Parse(c.APIURL); err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return errors.New("stack.api_url must be a valid http/https URL")
	}
	if !filepath.IsAbs(c.Dir) {
		return errors.New("stack.dir must be an absolute path")
	}
	if !projectPattern.MatchString(c.ProjectName) {
		return errors.New("stack.project_name must match [a-z0-9][a-z0-9_-]*")
	}
	if c.CosignPublicKey == "" {
		return errors.New("stack.cosign_public_key is required: images are only deployed when their signature verifies")
	}
	if filepath.Base(c.LockAsset) != c.LockAsset {
		return errors.New("stack.lock_asset must be a file name")
	}
	for _, f := range append(append([]string{}, c.ComposeFiles...), c.EnvFiles...) {
		if filepath.IsAbs(f) || escapes(filepath.ToSlash(f)) {
			return fmt.Errorf("stack: %q must be a path inside the release", f)
		}
	}
	for _, list := range [][]string{c.Services, c.ExcludeServices, c.ConfirmServices, c.Profiles} {
		for _, s := range list {
			if !servicePattern.MatchString(s) {
				return fmt.Errorf("stack: invalid service or profile name %q", s)
			}
		}
	}
	if c.HealthCheckURL != "" {
		if u, err := url.Parse(c.HealthCheckURL); err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
			return errors.New("stack.health_check_url must be a valid http/https URL")
		}
	}
	return nil
}

func validateStackPayload(p DeployPayload) string {
	if p.ArtifactName != "" || p.ArtifactDigest != "" {
		return "artifact_name and artifact_digest are not valid with kind stack"
	}
	if !tagPattern.MatchString(p.Tag) {
		return "tag is required with kind stack and must match [A-Za-z0-9][A-Za-z0-9._+-]*"
	}
	for _, s := range p.Confirm {
		if !servicePattern.MatchString(s) {
			return "invalid service name in confirm"
		}
	}
	return ""
}

// StackDeployer deploys docker stack releases. It implements Deployer for payloads of kind stack.
type StackDeployer struct {
	cfg   StackConfig
	hc    HealthCheck
	run   Runner
	http  *http.Client
	token string
}

func newStackDeployer(cfg Config, run Runner) *StackDeployer {
	return &StackDeployer{
		cfg:   cfg.Stack,
		hc:    cfg.HealthCheck,
		run:   run,
		http:  &http.Client{Timeout: 10 * time.Minute},
		token: os.Getenv(cfg.Stack.TokenEnv),
	}
}

// checkTools fails early when docker, cosign or the public key are missing.
func (d *StackDeployer) checkTools() error {
	for _, bin := range []string{d.cfg.DockerBinary, d.cfg.CosignBinary} {
		if _, err := exec.LookPath(bin); err != nil {
			return fmt.Errorf("%s not found: %w", bin, err)
		}
	}
	if _, err := os.Stat(d.cfg.CosignPublicKey); err != nil {
		return fmt.Errorf("cosign public key: %w", err)
	}
	return nil
}

func (d *StackDeployer) releasesDir() string { return filepath.Join(d.cfg.Dir, "releases") }
func (d *StackDeployer) currentLink() string { return filepath.Join(d.cfg.Dir, "current") }

// currentRelease returns the release dir the current symlink points to and the tag it was deployed from.
func (d *StackDeployer) currentRelease() (dir, tag string) {
	target, err := os.Readlink(d.currentLink())
	if err != nil {
		return "", ""
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(d.cfg.Dir, target)
	}
	b, _ := os.ReadFile(filepath.Join(target, stackTagFile))
	return target, strings.TrimSpace(string(b))
}

func (d *StackDeployer) Deploy(ctx context.Context, payload DeployPayload) (err error) {
	tag := payload.Tag
	previous, previousTag := d.currentRelease()
	if previousTag == tag {
		log.Printf("stack %s: already current (%s)", tag, previous)
		return nil
	}

	log.Printf("stack %s: start (repo=%s, current=%s)", tag, d.cfg.Repo, previousTag)
	defer func() {
		if err != nil {
			log.Printf("stack %s: failed: %v", tag, err)
		}
	}()

	release, err := d.fetchRelease(ctx, tag)
	if err != nil {
		return err
	}
	if err := d.checkRelease(release, tag); err != nil {
		return err
	}

	if err := d.run.ID.mkdirAll(d.cfg.Dir, 0o750); err != nil {
		return fmt.Errorf("create stack dir: %w", err)
	}
	if err := ensureDiskSpace(d.cfg.Dir, d.cfg.MinimumDiskSpaceGB); err != nil {
		return err
	}

	releaseDir, err := d.prepareRelease(ctx, release, tag)
	if err != nil {
		return err
	}
	switched := false
	defer func() {
		if err == nil {
			return
		}
		if !switched {
			_ = os.RemoveAll(releaseDir)
			return
		}
		err = errors.Join(err, d.rollback(previous, tag))
	}()

	if err := d.checkConfirm(previous, releaseDir, payload.Confirm); err != nil {
		return err
	}
	services, err := d.services(ctx, releaseDir)
	if err != nil {
		return err
	}
	if err := d.login(ctx); err != nil {
		return err
	}
	if err := d.compose(ctx, releaseDir, append([]string{"pull", "--quiet"}, services...)...); err != nil {
		return fmt.Errorf("pull: %w", err)
	}
	log.Printf("stack %s: pulled %s", tag, strings.Join(services, " "))

	for _, command := range d.cfg.PreUpCommands {
		if err := d.run.Shell(ctx, command, releaseDir); err != nil {
			return fmt.Errorf("pre-up command failed: %w", err)
		}
	}

	if err := d.run.ID.symlink(releaseDir, d.currentLink()); err != nil {
		return err
	}
	switched = true
	log.Printf("stack %s: switched %s -> %s", tag, d.currentLink(), releaseDir)

	if err := d.up(ctx, releaseDir, services); err != nil {
		return fmt.Errorf("up: %w", err)
	}
	if d.cfg.HealthCheckURL != "" {
		if err := d.healthCheck(ctx, tag); err != nil {
			return err
		}
	}
	for _, command := range d.cfg.PostUpCommands {
		if err := d.run.Shell(ctx, command, releaseDir); err != nil {
			return fmt.Errorf("post-up command failed: %w", err)
		}
	}

	log.Printf("stack %s: success", tag)
	if err := pruneReleases(d.releasesDir(), d.cfg.KeepReleases, releaseDir, previous); err != nil {
		log.Printf("stack %s: prune releases: %v", tag, err)
	}
	return nil
}

// prepareRelease builds releases/<tag> from the source tarball and the lock asset, verifies every image
// signature and links the shared files. Nothing is left behind on error.
func (d *StackDeployer) prepareRelease(ctx context.Context, release githubRelease, tag string) (releaseDir string, err error) {
	stagingDir, err := makeStagingDir(d.run.ID, d.releasesDir(), tag)
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

	tarball := filepath.Join(stagingDir, ".source.tar.gz")
	if err := d.download(ctx, d.apiURL("repos", d.cfg.Repo, "tarball", tag), "", tarball); err != nil {
		return "", fmt.Errorf("download source: %w", err)
	}
	// GitHub tarballs have one top dir <owner>-<repo>-<sha>/
	if err := extractArchive(ctx, d.run, tarball, stagingDir, 1); err != nil {
		return "", err
	}
	if err := os.Remove(tarball); err != nil {
		return "", err
	}

	asset := release.asset(d.cfg.LockAsset)
	if asset == nil {
		return "", fmt.Errorf("release %s has no asset %s", tag, d.cfg.LockAsset)
	}
	lockPath := filepath.Join(stagingDir, d.cfg.LockAsset)
	if err := os.RemoveAll(lockPath); err != nil {
		return "", err
	}
	if err := d.download(ctx, asset.URL, "application/octet-stream", lockPath); err != nil {
		return "", fmt.Errorf("download %s: %w", d.cfg.LockAsset, err)
	}
	images, err := readLock(lockPath)
	if err != nil {
		return "", err
	}
	if err := d.verifyImages(ctx, images); err != nil {
		return "", err
	}

	target := uniqueReleaseDir(d.releasesDir(), tag, time.Now())
	if err := os.Rename(stagingDir, target); err != nil {
		return "", fmt.Errorf("move release into place: %w", err)
	}
	releaseDir = target
	if err := d.writeOwned(filepath.Join(releaseDir, stackTagFile), tag+"\n"); err != nil {
		return "", err
	}
	if err := linkSharedPaths(d.run.ID, releaseDir, d.cfg.SharedLinks); err != nil {
		return "", err
	}
	for _, f := range d.cfg.EnvFiles {
		if _, err := os.Stat(filepath.Join(releaseDir, f)); err != nil {
			return "", fmt.Errorf("env file %s: %w (add it to stack.shared_links when it lives on the server)", f, err)
		}
	}
	return releaseDir, nil
}

func (d *StackDeployer) writeOwned(path, content string) error {
	// the release may ship a file or symlink with this name: replace it, never write through it
	if err := os.RemoveAll(path); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(content), 0o640); err != nil {
		return err
	}
	return d.run.ID.own(path)
}

func (d *StackDeployer) checkRelease(r githubRelease, tag string) error {
	if r.TagName != tag {
		return fmt.Errorf("release tag %q does not match %q", r.TagName, tag)
	}
	if r.Draft {
		return fmt.Errorf("release %s is a draft", tag)
	}
	if r.Prerelease && !d.cfg.AllowPrerelease {
		return fmt.Errorf("release %s is a prerelease (stack.allow_prerelease is false)", tag)
	}
	if d.cfg.RequireImmutable && !r.Immutable {
		return fmt.Errorf("release %s is not immutable (stack.require_immutable is true)", tag)
	}
	return nil
}

// readLock parses the lock asset (services.<name>.image) and requires every image pinned by digest.
func readLock(path string) (map[string]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var lock struct {
		Services map[string]struct {
			Image string `yaml:"image"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(b, &lock); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	if len(lock.Services) == 0 {
		return nil, fmt.Errorf("%s lists no images", filepath.Base(path))
	}
	images := make(map[string]string, len(lock.Services))
	for svc, s := range lock.Services {
		if !pinnedPattern.MatchString(s.Image) || strings.HasPrefix(s.Image, "-") {
			return nil, fmt.Errorf("%s: image of %s is not pinned by digest: %q", filepath.Base(path), svc, s.Image)
		}
		images[svc] = s.Image
	}
	return images, nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// verifyImages checks the cosign signature (and SBOM attestation) of every image with the configured key.
func (d *StackDeployer) verifyImages(ctx context.Context, images map[string]string) error {
	var failed []string
	for _, svc := range sortedKeys(images) {
		image := images[svc]
		args := append(append([]string{"verify", "--key", d.cfg.CosignPublicKey}, d.cfg.CosignArgs...), image)
		if err := d.run.Run(ctx, "", nil, d.cfg.CosignBinary, args...); err != nil {
			log.Printf("stack: %s: signature check failed: %v", svc, err)
			failed = append(failed, svc)
			continue
		}
		if d.cfg.RequireSBOM {
			args := append(append([]string{"verify-attestation", "--key", d.cfg.CosignPublicKey, "--type", "spdxjson"}, d.cfg.CosignArgs...), image)
			if err := d.run.Run(ctx, "", nil, d.cfg.CosignBinary, args...); err != nil {
				log.Printf("stack: %s: SBOM attestation check failed: %v", svc, err)
				failed = append(failed, svc)
			}
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("image verification failed: %s", strings.Join(failed, " "))
	}
	return nil
}

// checkConfirm refuses image changes of stack.confirm_services that the payload did not confirm.
func (d *StackDeployer) checkConfirm(previous, releaseDir string, confirmed []string) error {
	if len(d.cfg.ConfirmServices) == 0 {
		return nil
	}
	next, err := readLock(filepath.Join(releaseDir, d.cfg.LockAsset))
	if err != nil {
		return err
	}
	var old map[string]string
	if previous != "" {
		old, _ = readLock(filepath.Join(previous, d.cfg.LockAsset))
	}
	var missing []string
	for _, svc := range d.cfg.ConfirmServices {
		image, ok := next[svc]
		if !ok || old[svc] == image {
			continue
		}
		if !slices.Contains(confirmed, svc) {
			missing = append(missing, svc)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("image change of %s needs confirmation: send \"confirm\": [%q] after a backup", strings.Join(missing, ", "), strings.Join(missing, `", "`))
	}
	return nil
}

func (d *StackDeployer) composeArgs(releaseDir string, args ...string) []string {
	out := []string{"compose", "--project-name", d.cfg.ProjectName, "--project-directory", releaseDir}
	for _, f := range d.cfg.ComposeFiles {
		out = append(out, "-f", filepath.Join(releaseDir, f))
	}
	out = append(out, "-f", filepath.Join(releaseDir, d.cfg.LockAsset))
	for _, f := range d.cfg.EnvFiles {
		out = append(out, "--env-file", filepath.Join(releaseDir, f))
	}
	for _, p := range d.cfg.Profiles {
		out = append(out, "--profile", p)
	}
	return append(out, args...)
}

func (d *StackDeployer) compose(ctx context.Context, releaseDir string, args ...string) error {
	return d.run.Run(ctx, releaseDir, nil, d.cfg.DockerBinary, d.composeArgs(releaseDir, args...)...)
}

// services validates the compose config and returns the services to pull and start: stack.services, or
// every service minus stack.exclude_services and minus services of profiles that are not in stack.profiles
// (naming a service on "up" would start it even when its profile is off).
func (d *StackDeployer) services(ctx context.Context, releaseDir string) ([]string, error) {
	if err := d.compose(ctx, releaseDir, "config", "--quiet"); err != nil {
		return nil, fmt.Errorf("compose config: %w", err)
	}
	if len(d.cfg.Services) > 0 {
		return d.cfg.Services, nil
	}
	out, err := d.run.Output(ctx, releaseDir, nil, d.cfg.DockerBinary, d.composeArgs(releaseDir, "config", "--format", "json")...)
	if err != nil {
		return nil, fmt.Errorf("compose config: %w", err)
	}
	var config struct {
		Services map[string]struct {
			Profiles []string `json:"profiles"`
		} `json:"services"`
	}
	if err := json.Unmarshal(out, &config); err != nil {
		return nil, fmt.Errorf("compose config: %w", err)
	}
	var services []string
	for name, svc := range config.Services {
		if slices.Contains(d.cfg.ExcludeServices, name) {
			continue
		}
		active := len(svc.Profiles) == 0
		for _, p := range svc.Profiles {
			active = active || slices.Contains(d.cfg.Profiles, p)
		}
		if active {
			services = append(services, name)
		}
	}
	if len(services) == 0 {
		return nil, errors.New("no services to deploy")
	}
	sort.Strings(services)
	return services, nil
}

func (d *StackDeployer) up(ctx context.Context, releaseDir string, services []string) error {
	args := []string{"up", "--detach", "--no-build", "--wait", "--wait-timeout", strconv.FormatInt(d.cfg.WaitTimeoutSeconds, 10)}
	if d.cfg.RemoveOrphans {
		args = append(args, "--remove-orphans")
	}
	return d.compose(ctx, releaseDir, append(args, services...)...)
}

// login runs docker login with the GitHub token when stack.registry_username is set.
func (d *StackDeployer) login(ctx context.Context) error {
	if d.cfg.RegistryUsername == "" {
		return nil
	}
	if d.token == "" {
		return fmt.Errorf("stack.registry_username is set but %s is empty", d.cfg.TokenEnv)
	}
	cmd := d.run.command(ctx, "", nil, d.cfg.DockerBinary, "login", d.cfg.Registry, "--username", d.cfg.RegistryUsername, "--password-stdin")
	cmd.Stdin = strings.NewReader(d.token)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("docker login %s: %w: %s", d.cfg.Registry, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// rollback points current back to the previous release and starts it again. Without a previous release
// the current link is removed and the running containers are left as they are.
func (d *StackDeployer) rollback(previous, tag string) error {
	if previous == "" {
		if err := os.Remove(d.currentLink()); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("rollback: remove current link: %w", err)
		}
		log.Printf("stack %s: rollback: removed %s (no previous release, containers left as they are)", tag, d.currentLink())
		return nil
	}
	if err := d.run.ID.symlink(previous, d.currentLink()); err != nil {
		return fmt.Errorf("rollback: switch to previous release: %w", err)
	}
	log.Printf("stack %s: rollback: switched %s -> %s", tag, d.currentLink(), previous)

	// fresh context: the deploy context may already be expired
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(d.cfg.RollbackTimeoutSeconds)*time.Second)
	defer cancel()
	var errs []error
	if services, err := d.services(ctx, previous); err != nil {
		errs = append(errs, fmt.Errorf("rollback: %w", err))
	} else if err := d.up(ctx, previous, services); err != nil {
		errs = append(errs, fmt.Errorf("rollback: up: %w", err))
	} else {
		log.Printf("stack %s: rollback: previous release is up", tag)
	}
	for _, command := range d.cfg.RollbackCommands {
		if err := d.run.Shell(ctx, command, previous); err != nil {
			errs = append(errs, fmt.Errorf("rollback command failed: %w", err))
		}
	}
	for _, e := range errs {
		log.Printf("stack %s: %v", tag, e)
	}
	return errors.Join(errs...)
}

func (d *StackDeployer) healthCheck(ctx context.Context, tag string) error {
	client := &http.Client{Timeout: time.Duration(d.hc.TimeoutSeconds) * time.Second}
	var err error
	for attempt := 1; attempt <= d.hc.Attempts; attempt++ {
		if err = d.healthCheckOnce(ctx, client, tag); err == nil {
			return nil
		}
		log.Printf("stack %s: health check attempt %d/%d: %v", tag, attempt, d.hc.Attempts, err)
		if attempt == d.hc.Attempts {
			break
		}
		select {
		case <-ctx.Done():
			return errors.Join(err, ctx.Err())
		case <-time.After(time.Duration(d.hc.RetryIntervalSeconds) * time.Second):
		}
	}
	return err
}

func (d *StackDeployer) healthCheckOnce(ctx context.Context, client *http.Client, tag string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.cfg.HealthCheckURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", fmt.Sprintf("%s for stack %s", d.hc.UserAgentPrefix, tag))
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("health check failed: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != d.hc.ExpectedStatus {
		return fmt.Errorf("health check returned %d", resp.StatusCode)
	}
	return nil
}

// --- GitHub ---

type githubRelease struct {
	TagName    string        `json:"tag_name"`
	Draft      bool          `json:"draft"`
	Prerelease bool          `json:"prerelease"`
	Immutable  bool          `json:"immutable"`
	Assets     []githubAsset `json:"assets"`
}

type githubAsset struct {
	Name string `json:"name"`
	// URL is the API URL of the asset; with Accept: application/octet-stream it redirects to the file.
	URL string `json:"url"`
}

func (r githubRelease) asset(name string) *githubAsset {
	for i := range r.Assets {
		if r.Assets[i].Name == name {
			return &r.Assets[i]
		}
	}
	return nil
}

func (d *StackDeployer) apiURL(parts ...string) string {
	escaped := make([]string, 0, len(parts))
	for _, p := range parts {
		if p == d.cfg.Repo {
			escaped = append(escaped, p) // owner/name, validated
			continue
		}
		escaped = append(escaped, url.PathEscape(p))
	}
	return strings.TrimRight(d.cfg.APIURL, "/") + "/" + strings.Join(escaped, "/")
}

// get sends an authenticated GitHub request. The token is only sent to the API host:
// net/http drops the Authorization header on redirects to another host (codeload, release asset storage).
func (d *StackDeployer) get(ctx context.Context, rawURL, accept string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	if accept == "" {
		accept = "application/vnd.github+json"
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "magenx-deputy-go")
	if d.token != "" {
		req.Header.Set("Authorization", "Bearer "+d.token)
	}
	resp, err := d.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		return nil, fmt.Errorf("GET %s: %s: %s", req.URL.Redacted(), resp.Status, strings.TrimSpace(string(body)))
	}
	return resp, nil
}

func (d *StackDeployer) getJSON(ctx context.Context, rawURL string, v any) error {
	resp, err := d.get(ctx, rawURL, "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(v)
}

func (d *StackDeployer) fetchRelease(ctx context.Context, tag string) (githubRelease, error) {
	var r githubRelease
	if err := d.getJSON(ctx, d.apiURL("repos", d.cfg.Repo, "releases", "tags", tag), &r); err != nil {
		return r, fmt.Errorf("release %s: %w", tag, err)
	}
	return r, nil
}

// latestTag returns the newest release that is neither draft nor prerelease.
func (d *StackDeployer) latestTag(ctx context.Context) (string, error) {
	var r githubRelease
	if err := d.getJSON(ctx, d.apiURL("repos", d.cfg.Repo, "releases", "latest"), &r); err != nil {
		return "", err
	}
	if !tagPattern.MatchString(r.TagName) {
		return "", fmt.Errorf("latest release has an invalid tag %q", r.TagName)
	}
	return r.TagName, nil
}

// download writes a GitHub URL to destination, owned by the run_as user.
func (d *StackDeployer) download(ctx context.Context, rawURL, accept, destination string) error {
	resp, err := d.get(ctx, rawURL, accept)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	out, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, resp.Body)
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return d.run.ID.own(destination)
}

// --- polling ---

// pollStack deploys the latest stack release every interval. A tag that failed is not retried until a
// newer release appears; send the webhook to retry it.
func (s *Server) pollStack(ctx context.Context, d *StackDeployer, every time.Duration) {
	log.Printf("stack: polling %s every %s", d.cfg.Repo, every)
	failed := ""
	for {
		s.pollStackOnce(ctx, d, &failed)
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}

func (s *Server) pollStackOnce(ctx context.Context, d *StackDeployer, failed *string) {
	lookupCtx, cancel := context.WithTimeout(ctx, time.Minute)
	tag, err := d.latestTag(lookupCtx)
	cancel()
	if err != nil {
		log.Printf("stack: poll: %v", err)
		return
	}
	if tag == *failed {
		return
	}
	if _, current := d.currentRelease(); current == tag {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	deployCtx, cancel := context.WithTimeout(ctx, time.Duration(s.cfg.Stack.DeploymentTimeoutSeconds)*time.Second)
	defer cancel()
	if err := d.Deploy(deployCtx, DeployPayload{Kind: "stack", Tag: tag}); err != nil {
		*failed = tag
		return
	}
	*failed = ""
}
