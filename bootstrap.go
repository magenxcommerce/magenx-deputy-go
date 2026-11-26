package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
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
	if err := ensureWebhookStopped(cfg.ListenAddr); err != nil {
		log.Printf("bootstrap: %v", err)
		return 1
	}
	server, err := buildServer(cfg)
	if err != nil {
		log.Printf("bootstrap: %v", err)
		return 1
	}
	if err := bootstrap(context.Background(), cfg, server, opts); err != nil {
		log.Printf("bootstrap: %v", err)
		return 1
	}
	log.Printf("bootstrap: done")
	return 0
}

// ensureWebhookStopped refuses to run next to the webhook service: both would deploy into the same dirs.
func ensureWebhookStopped(listenAddr string) error {
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return fmt.Errorf("%s is in use, is the deputy service running? Stop it first, or deploy through the webhook: %w", listenAddr, err)
	}
	return ln.Close()
}

func bootstrap(ctx context.Context, cfg Config, s *Server, opts bootstrapOptions) error {
	s.mu.Lock()
	defer s.mu.Unlock()

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
	log.Printf("bootstrap: %s:%s is %s (%s)", a.Repository, tag, a.Version, a.LayerDigest)
	return a, nil
}

func bootstrapStack(ctx context.Context, cfg Config, s *Server, opts bootstrapOptions) error {
	if cfg.Stack.Source != sourceOCI {
		return errors.New("bootstrap needs stack.source oci: S3 has no moving tag (or use -skip-stack)")
	}
	if cfg.Stack.AllowUnsignedBundle {
		return errors.New("bootstrap needs the bundle signature check: set stack.allow_unsigned_bundle false")
	}
	a, err := resolveRelease(ctx, ociClient{cfg: cfg.OCI}, cfg.Stack.OCIRepository, opts.StackTag)
	if err != nil {
		return err
	}
	payload := DeployPayload{Kind: "stack", Tag: a.Version, ArtifactDigest: a.LayerDigest, Confirm: opts.Confirm}
	if msg := s.validateStackPayload(&payload); msg != "" {
		return errors.New(msg)
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(cfg.Stack.DeploymentTimeoutSeconds)*time.Second)
	defer cancel()
	return s.stack.Deploy(ctx, payload)
}

func bootstrapCode(ctx context.Context, cfg Config, s *Server, opts bootstrapOptions) error {
	if cfg.Deploy.Source != sourceOCI {
		return errors.New("bootstrap needs deploy.source oci: S3 has no moving tag (or use -skip-code)")
	}
	if cfg.Deploy.CosignPublicKey == "" {
		return errors.New("bootstrap needs the release signature check: set deploy.cosign_public_key")
	}
	a, err := resolveRelease(ctx, ociClient{cfg: cfg.OCI}, cfg.Deploy.OCIRepository, opts.CodeTag)
	if err != nil {
		return err
	}
	if current := currentCodeRelease(cfg); current == a.Version {
		log.Printf("bootstrap: code %s already current", a.Version)
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

// currentCodeRelease is the name of the release dir the code current link points to, "" without one.
func currentCodeRelease(cfg Config) string {
	link := filepath.Join(cfg.Deploy.RootDir, cfg.Deploy.DefaultBrand, filepath.FromSlash(cfg.Deploy.CurrentLinkSuffix))
	target, err := os.Readlink(link)
	if err != nil {
		return ""
	}
	return filepath.Base(target)
}
