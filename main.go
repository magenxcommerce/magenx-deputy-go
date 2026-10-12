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
	"os/exec"
	"path/filepath"
	"regexp"
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
}

type HealthCheck struct {
	Enabled         bool   `json:"enabled" yaml:"enabled"`
	Scheme          string `json:"scheme" yaml:"scheme"`
	PathTemplate    string `json:"path_template" yaml:"path_template"`
	ExpectedStatus  int    `json:"expected_status" yaml:"expected_status"`
	TimeoutSeconds  int64  `json:"timeout_seconds" yaml:"timeout_seconds"`
	UserAgentPrefix string `json:"user_agent_prefix" yaml:"user_agent_prefix"`
}

type SharedTarget struct {
	Source string `json:"source" yaml:"source"`
	Target string `json:"target" yaml:"target"`
}

type DeployPayload struct {
	ArtifactName   string `json:"artifact_name" yaml:"artifact_name"`
	ArtifactDigest string `json:"artifact_digest" yaml:"artifact_digest"`
	Brand          string `json:"brand" yaml:"brand"`
	Domain         string `json:"domain" yaml:"domain"`
	Timestamp      int64  `json:"timestamp" yaml:"timestamp"`
}

type Deployer interface {
	Deploy(context.Context, DeployPayload) error
}

type Server struct {
	cfg      Config
	deployer Deployer
	mu       sync.Mutex
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

	srv := &http.Server{
		Addr:    cfg.ListenAddr,
		Handler: &Server{cfg: cfg, deployer: &S3MagentoDeployer{cfg: cfg}},
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
	brand, err := requireMatchOrEmpty(payload.Brand, s.cfg.Deploy.DefaultBrand)
	if err != nil {
		http.Error(w, "brand does not match configured default", http.StatusBadRequest)
		return
	}
	domain, err := requireMatchOrEmpty(payload.Domain, s.cfg.Deploy.DefaultDomain)
	if err != nil {
		http.Error(w, "domain does not match configured default", http.StatusBadRequest)
		return
	}
	payload.Brand = brand
	payload.Domain = domain
	if payload.ArtifactName == "" || payload.Brand == "" {
		http.Error(w, "artifact_name and brand are required", http.StatusBadRequest)
		return
	}
	if s.cfg.HealthCheck.Enabled && payload.Domain == "" {
		http.Error(w, "domain is required when health_check is enabled", http.StatusBadRequest)
		return
	}
	if !isValidPayloadPathFields(payload.Brand, payload.ArtifactName) {
		http.Error(w, "invalid brand or artifact_name format", http.StatusBadRequest)
		return
	}
	if payload.ArtifactDigest == "" && !s.cfg.AllowMissingArtifactDigest {
		http.Error(w, "artifact_digest is required", http.StatusBadRequest)
		return
	}
	if payload.ArtifactDigest != "" && !digestPattern.MatchString(payload.ArtifactDigest) {
		http.Error(w, "invalid artifact_digest format, expected sha256:<hex>", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(s.cfg.DeploymentTimeoutSeconds)*time.Second)
	defer cancel()

	if err := s.deployer.Deploy(ctx, payload); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
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
}

func (d *S3MagentoDeployer) Deploy(ctx context.Context, payload DeployPayload) (err error) {
	artifactFile := filepath.Base(payload.ArtifactName)
	releaseVersion := normalizeReleaseVersion(artifactFile)
	deployDir := filepath.Join(d.cfg.Deploy.RootDir, payload.Brand)
	releasesDir := filepath.Join(deployDir, d.cfg.Deploy.ReleasesDirName)
	releaseDir := filepath.Join(releasesDir, releaseVersion)
	currentLink := filepath.Join(deployDir, filepath.FromSlash(d.cfg.Deploy.CurrentLinkSuffix))

	if err := ensureDiskSpace(deployDir, d.cfg.Deploy.MinimumDiskSpaceGB); err != nil {
		return err
	}
	if err := os.MkdirAll(releaseDir, 0o750); err != nil {
		return fmt.Errorf("create release dir: %w", err)
	}

	artifactPath := filepath.Join(releaseDir, artifactFile)
	actualDigest, err := d.downloadArtifact(ctx, payload.Brand, artifactFile, artifactPath)
	if err != nil {
		return err
	}
	if err := verifyArtifactDigest(payload.ArtifactDigest, actualDigest); err != nil {
		_ = os.Remove(artifactPath)
		return err
	}
	if err := extractArtifact(ctx, artifactPath, releaseDir); err != nil {
		return err
	}
	if err := os.Remove(artifactPath); err != nil {
		return fmt.Errorf("remove downloaded artifact: %w", err)
	}

	if err := linkSharedPaths(releaseDir, d.cfg.SharedLinks); err != nil {
		return err
	}

	previousRelease, _ := os.Readlink(currentLink)
	switched := false
	defer func() {
		if err == nil || !switched {
			return
		}
		if previousRelease == "" {
			_ = os.Remove(currentLink)
			return
		}
		_ = forceSymlink(previousRelease, currentLink)
	}()

	if err := forceSymlink(releaseDir, currentLink); err != nil {
		return err
	}
	switched = true

	if d.isLeader() {
		for _, command := range d.cfg.Deploy.LeaderCommands {
			if err := runCommand(ctx, "/bin/sh", "-c", command); err != nil {
				return fmt.Errorf("leader command failed: %w", err)
			}
		}
	}

	if d.cfg.HealthCheck.Enabled {
		if err := d.healthCheck(ctx, payload.Domain, releaseVersion); err != nil {
			return err
		}
	}

	return nil
}

func (d *S3MagentoDeployer) isLeader() bool {
	value := strings.ToLower(strings.TrimSpace(os.Getenv(d.cfg.Deploy.LeaderEnvVar)))
	if value == "" {
		return d.cfg.Deploy.LeaderDefault
	}
	return value == "1" || value == "true" || value == "yes"
}

func (d *S3MagentoDeployer) healthCheck(ctx context.Context, domain, releaseVersion string) error {
	if domain == "" {
		return errors.New("domain is required for health check")
	}
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
	bucket := d.cfg.Deploy.S3Bucket
	if d.cfg.Deploy.S3BucketPerBrand {
		bucket = brand
	}
	if bucket == "" {
		return "", errors.New("s3 bucket is not configured")
	}

	prefix := strings.Trim(d.cfg.Deploy.S3Prefix, "/")
	artifactPath := artifact
	if prefix != "" {
		artifactPath = prefix + "/" + artifact
	}
	s3URI := fmt.Sprintf("s3://%s/%s", bucket, artifactPath)

	loadOptions := []func(*sdkconfig.LoadOptions) error{
		sdkconfig.WithRegion(d.cfg.Deploy.S3Region),
	}
	if d.cfg.Deploy.S3Endpoint != "" {
		resolver := awsv2.EndpointResolverWithOptionsFunc(func(service, region string, options ...interface{}) (awsv2.Endpoint, error) {
			if service == s3.ServiceID {
				return awsv2.Endpoint{
					URL:               d.cfg.Deploy.S3Endpoint,
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
		o.UsePathStyle = d.cfg.Deploy.S3UsePathStyle
	})

	obj, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: awsv2.String(bucket),
		Key:    awsv2.String(artifactPath),
	})
	if err != nil {
		return "", fmt.Errorf("download artifact %s: %w", s3URI, err)
	}
	defer obj.Body.Close()

	out, err := os.Create(destination)
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
		if os.IsNotExist(err) {
			if err := os.MkdirAll(path, 0o750); err != nil {
				return fmt.Errorf("create deploy dir: %w", err)
			}
			if err := syscall.Statfs(path, &stat); err != nil {
				return fmt.Errorf("read filesystem stats: %w", err)
			}
		} else {
			return fmt.Errorf("read filesystem stats: %w", err)
		}
	}
	available := uint64(stat.Bavail) * uint64(stat.Bsize)
	required := requiredGB * 1024 * 1024 * 1024
	if available < required {
		return fmt.Errorf("not enough disk space: required=%dGB available=%dGB", requiredGB, available/(1024*1024*1024))
	}
	return nil
}

func forceSymlink(target, linkPath string) error {
	if err := os.MkdirAll(filepath.Dir(linkPath), 0o750); err != nil {
		return err
	}
	tmp := linkPath + ".next"
	_ = os.Remove(tmp)
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	return os.Rename(tmp, linkPath)
}

func linkSharedPaths(releaseDir string, links []SharedTarget) error {
	for _, shared := range links {
		if shared.Source == "" || shared.Target == "" {
			return errors.New("shared link source and target are required")
		}
		targetPath, err := safeJoin(releaseDir, filepath.FromSlash(strings.TrimPrefix(shared.Target, "/")))
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(targetPath), 0o750); err != nil {
			return err
		}
		if err := os.RemoveAll(targetPath); err != nil {
			return err
		}
		if err := os.Symlink(shared.Source, targetPath); err != nil {
			return err
		}
	}
	return nil
}

func runCommand(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		if len(output) == 0 {
			return err
		}
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
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

func extractArtifact(ctx context.Context, src, dst string) error {
	return runCommand(ctx, "tar", "-xzf", src, "-C", dst)
}

func exceedsTimestampWindow(now, timestamp, window int64) bool {
	if now >= timestamp {
		return now-timestamp > window
	}
	return timestamp-now > window
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
	return nil
}
