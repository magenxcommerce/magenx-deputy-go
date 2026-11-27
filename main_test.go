package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func TestCopyWithSHA256(t *testing.T) {
	var dst bytes.Buffer
	digest, err := copyWithSHA256(&dst, strings.NewReader("artifact"))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("artifact"))
	if want := "sha256:" + hex.EncodeToString(sum[:]); digest != want {
		t.Fatalf("digest=%s want=%s", digest, want)
	}
	if dst.String() != "artifact" {
		t.Fatalf("copied=%q", dst.String())
	}
}

func TestVerifyArtifactDigest(t *testing.T) {
	sum := sha256.Sum256([]byte("artifact"))
	actual := "sha256:" + hex.EncodeToString(sum[:])

	if err := verifyArtifactDigest(actual, actual); err != nil {
		t.Fatalf("match: %v", err)
	}
	if err := verifyArtifactDigest(strings.ToUpper(actual[:7])+strings.ToUpper(actual[7:]), actual); err != nil {
		t.Fatalf("case-insensitive match: %v", err)
	}
	if err := verifyArtifactDigest("", actual); err == nil {
		t.Fatal("empty expected digest accepted")
	}
	if err := verifyArtifactDigest("sha256:"+strings.Repeat("0", 64), actual); err == nil {
		t.Fatal("mismatch not detected")
	}
}

func TestExampleConfigLoads(t *testing.T) {
	cfg, err := loadConfig("config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Deploy.RunAs != "mystore" || cfg.Stack.Enabled {
		t.Fatalf("run_as=%q stack.enabled=%v", cfg.Deploy.RunAs, cfg.Stack.Enabled)
	}
	cfg.Stack.Enabled = true
	if err := cfg.validate(); err != nil {
		t.Fatalf("example stack section: %v", err)
	}
}
