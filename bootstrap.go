package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// bootstrap deploys the release a moving tag (e.g. production) points to, on a new server and without a
// webhook: first the docker stack, then the Magento code. Both must come from source oci. The tag is resolved
// to its manifest, the manifest annotation org.opencontainers.image.version names the release, and the same
// Deploy as for a webhook runs with <repository>:<version> and the layer digest of the moving tag, so the
// version tag must hold the same archive. Nothing here is signed by the webhook HMAC, so bootstrap requires
// cosign checks: stack.allow_unsigned_bundle false and deploy.cosign_public_key set.

type bootstrapOptions struct {
	StackTag  string
	CodeTag   string
	SkipStack bool
	SkipCode  bool
	// Confirm is passed to the stack deploy: services of stack.confirm_services whose image may change.
	Confirm []string
}

func runBootstrap(args []string) int {
	fs := flag.NewFlagSet("bootstrap", flag.ContinueOnError)
	configPath := fs.String("config", "/etc/magenx-deputy-go/config.yaml", "Path to config YAML file")
	tag := fs.String("tag", "production", "moving image tag to deploy, for the stack and the code")
	stackTag := fs.String("stack-tag", "", "image tag of the stack bundle (default: -tag)")
	codeTag := fs.String("code-tag", "", "image tag of the Magento release (default: -tag)")
	skipStack := fs.Bool("skip-stack", false, "do not deploy the stack")
	skipCode := fs.Bool("skip-code", false, "do not deploy the Magento code")
	confirm := fs.String("confirm", "", "comma separated stack.confirm_services whose image may change")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: magenx-deputy-go bootstrap [flags]\n\n"+
			"Deploys the stack and the Magento release that a moving image tag points to, before the\n"+
			"webhook service runs (new server, cloud-init). Needs source oci and cosign signature checks.\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	opts := bootstrapOptions{StackTag: *stackTag, CodeTag: *codeTag, SkipStack: *skipStack, SkipCode: *skipCode}
	if opts.StackTag == "" {
		opts.StackTag = *tag
	}
	if opts.CodeTag == "" {
		opts.CodeTag = *tag
	}
	for _, svc := range strings.Split(*confirm, ",") {
		if svc = strings.TrimSpace(svc); svc != "" {
			opts.Confirm = append(opts.Confirm, svc)
		}
	}

	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Printf("bootstrap: failed to load config: %v", err)
		return 1
	}
	server, err := buildServer(cfg)
	if err != nil {
		log.Printf("bootstrap: %v", err)
		return 1
	}
	server.lockPath = *configPath
	if err := bootstrap(context.Background(), cfg, server, opts); err != nil {
		log.Printf("bootstrap: %v", err)
		return 1
	}
	log.Printf("bootstrap: done")
	return 0
}

func bootstrap(ctx context.Context, cfg Config, s *Server, opts bootstrapOptions) error {
	// a running service (webhook, watch) and bootstrap take turns
	unlock, err := s.lockDeploys()
	if err != nil {
		return err
	}
	defer unlock()

	if !opts.SkipStack && s.stack != nil {
		if err := bootstrapStack(ctx, cfg, s, opts); err != nil {
			return fmt.Errorf("stack: %w", err)
		}
	}
	if !opts.SkipCode {
		if err := bootstrapCode(ctx, cfg, s, opts); err != nil {
			return fmt.Errorf("code: %w", err)
		}
	}
	return nil
}

// resolveRelease turns repository:<moving tag> into its version and layer digest.
func resolveRelease(ctx context.Context, oci ociClient, repository, tag string) (ociArtifact, error) {
	if !isSafeName(tag) {
		return ociArtifact{}, fmt.Errorf("invalid tag %q", tag)
	}
	a, err := oci.resolve(ctx, repository, tag)
	if err != nil {
		return ociArtifact{}, err
	}
	if a.Version == "" {
		return ociArtifact{}, fmt.Errorf("%s:%s has no %s annotation: push the release with it (crane mutate --annotation)", a.Repository, tag, ociVersionAnnotation)
	}
	if !isSafeName(a.Version) {
		return ociArtifact{}, fmt.Errorf("%s:%s: invalid %s %q", a.Repository, tag, ociVersionAnnotation, logSafe(a.Version))
	}
	log.Printf("%s:%s is %s (%s)", a.Repository, tag, a.Version, a.LayerDigest)
	return a, nil
}

// stackFromRegistry fails unless the stack can be deployed from a moving tag: source oci and the bundle
// signature checked, the trust anchor when no webhook HMAC covers the digest (bootstrap, watch).
func stackFromRegistry(cfg StackConfig) error {
	if cfg.Source != sourceOCI {
		return errors.New("needs stack.source oci: S3 has no moving tag")
	}
	if cfg.AllowUnsignedBundle {
		return errors.New("needs the bundle signature check: set stack.allow_unsigned_bundle false")
	}
	return nil
}

// codeFromRegistry is stackFromRegistry for the Magento code: source oci and deploy.cosign_public_key.
func codeFromRegistry(cfg DeployConfig) error {
	if cfg.Source != sourceOCI {
		return errors.New("needs deploy.source oci: S3 has no moving tag")
	}
	if cfg.CosignPublicKey == "" {
		return errors.New("needs the release signature check: set deploy.cosign_public_key")
	}
	return nil
}

// deployStackRelease deploys the resolved stack release a like a webhook with its version and layer digest.
// The caller holds s.mu.
func deployStackRelease(ctx context.Context, cfg Config, s *Server, a ociArtifact, confirm []string) error {
	payload := DeployPayload{Kind: "stack", Tag: a.Version, ArtifactDigest: a.LayerDigest, Confirm: confirm}
	if msg := s.validateStackPayload(&payload); msg != "" {
		return errors.New(msg)
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(cfg.Stack.DeploymentTimeoutSeconds)*time.Second)
	defer cancel()
	return s.stack.Deploy(ctx, payload)
}

// deployCodeRelease deploys the resolved Magento release a, unless it is current already. The caller holds s.mu.
func deployCodeRelease(ctx context.Context, cfg Config, s *Server, a ociArtifact) error {
	if current := currentCodeRelease(cfg); current == a.Version {
		log.Printf("code %s already current", a.Version)
		return nil
	}
	payload := DeployPayload{Kind: "code", Tag: a.Version, ArtifactDigest: a.LayerDigest}
	if msg := s.validateCodePayload(&payload); msg != "" {
		return errors.New(msg)
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(cfg.DeploymentTimeoutSeconds)*time.Second)
	defer cancel()
	return s.deployer.Deploy(ctx, payload)
}

func bootstrapStack(ctx context.Context, cfg Config, s *Server, opts bootstrapOptions) error {
	if err := stackFromRegistry(cfg.Stack); err != nil {
		return fmt.Errorf("bootstrap %w (or use -skip-stack)", err)
	}
	a, err := resolveRelease(ctx, ociClient{cfg: cfg.OCI}, cfg.Stack.OCIRepository, opts.StackTag)
	if err != nil {
		return err
	}
	return deployStackRelease(ctx, cfg, s, a, opts.Confirm)
}

func bootstrapCode(ctx context.Context, cfg Config, s *Server, opts bootstrapOptions) error {
	if err := codeFromRegistry(cfg.Deploy); err != nil {
		return fmt.Errorf("bootstrap %w (or use -skip-code)", err)
	}
	a, err := resolveRelease(ctx, ociClient{cfg: cfg.OCI}, cfg.Deploy.OCIRepository, opts.CodeTag)
	if err != nil {
		return err
	}
	return deployCodeRelease(ctx, cfg, s, a)
}

// currentCodeRelease is the name of the release dir the code current link points to, "" without one.
func currentCodeRelease(cfg Config) string {
	link := filepath.Join(cfg.Deploy.RootDir, cfg.Deploy.DefaultBrand, filepath.FromSlash(cfg.Deploy.CurrentLinkSuffix))
	target, err := os.Readlink(link)
	if err != nil {
		return ""
	}
	return filepath.Base(target)
}
