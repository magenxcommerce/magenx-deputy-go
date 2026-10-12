package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// sha256 of "artifact"
const testDigest = "sha256:c7c5c1d70c5dec4416ab6158afd0b223ef40c29b1dc1f97ed9428b94d4cadb1c"

type fakeDeployer struct {
	calls   int
	last    DeployPayload
	deployE error
}

func (f *fakeDeployer) Deploy(_ context.Context, payload DeployPayload) error {
	f.calls++
	f.last = payload
	return f.deployE
}

func TestWebhookValidationAndDeployment(t *testing.T) {
	cfg := Config{
		WebhookPath:            "/deploy",
		WebhookSecret:          "secret",
		HMACKey:                "hmac",
		TimestampWindowSeconds: 300,
		Deploy: DeployConfig{
			DefaultBrand:  "demo",
			DefaultDomain: "shop.example",
		},
	}
	cfg.applyDefaults()

	now := time.Now().Unix()
	payload := []byte(fmt.Sprintf(`{"artifact_name":"release_2.4.7.tar.gz","artifact_digest":"%s","brand":"demo","domain":"shop.example","timestamp":%d}`, testDigest, now))

	tests := []struct {
		name           string
		secret         string
		signature      string
		timestamp      string
		deployerErr    error
		wantStatusCode int
		wantCalls      int
	}{
		{
			name:           "forbidden secret",
			secret:         "bad",
			signature:      sign("hmac", payload),
			timestamp:      strconv.FormatInt(now, 10),
			wantStatusCode: http.StatusForbidden,
			wantCalls:      0,
		},
		{
			name:           "bad signature",
			secret:         "secret",
			signature:      "sha256=deadbeef",
			timestamp:      strconv.FormatInt(now, 10),
			wantStatusCode: http.StatusForbidden,
			wantCalls:      0,
		},
		{
			name:           "expired timestamp",
			secret:         "secret",
			signature:      sign("hmac", payload),
			timestamp:      strconv.FormatInt(now-1000, 10),
			wantStatusCode: http.StatusForbidden,
			wantCalls:      0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			deployer := &fakeDeployer{}
			server := &Server{cfg: cfg, deployer: deployer}
			req := httptest.NewRequest(http.MethodPost, "/deploy", bytes.NewReader(payload))
			req.Header.Set("X-Webhook-Secret", tc.secret)
			req.Header.Set("X-Signature", tc.signature)
			req.Header.Set("X-Timestamp", tc.timestamp)
			res := httptest.NewRecorder()

			server.ServeHTTP(res, req)

			if res.Code != tc.wantStatusCode {
				t.Fatalf("status=%d want=%d", res.Code, tc.wantStatusCode)
			}
			if deployer.calls != tc.wantCalls {
				t.Fatalf("calls=%d want=%d", deployer.calls, tc.wantCalls)
			}
		})
	}
}

func TestWebhookSuccessAndFailure(t *testing.T) {
	now := time.Now().Unix()
	payload := []byte(`{"artifact_name":"release_2.4.7.tar.gz","artifact_digest":"` + testDigest + `","brand":"demo","domain":"shop.example","timestamp":` + strconv.FormatInt(now, 10) + `}`)

	cfg := Config{
		WebhookPath:            "/deploy",
		WebhookSecret:          "secret",
		HMACKey:                "hmac",
		TimestampWindowSeconds: 300,
		Deploy: DeployConfig{
			DefaultBrand:  "demo",
			DefaultDomain: "shop.example",
		},
	}
	cfg.applyDefaults()

	tests := []struct {
		name           string
		deployErr      error
		wantStatusCode int
	}{
		{name: "success", wantStatusCode: http.StatusOK},
		{name: "deployer failure", deployErr: context.DeadlineExceeded, wantStatusCode: http.StatusInternalServerError},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			deployer := &fakeDeployer{deployE: tc.deployErr}
			server := &Server{cfg: cfg, deployer: deployer}

			req := httptest.NewRequest(http.MethodPost, "/deploy", bytes.NewReader(payload))
			req.Header.Set("X-Webhook-Secret", "secret")
			req.Header.Set("X-Signature", sign("hmac", payload))
			req.Header.Set("X-Timestamp", strconv.FormatInt(now, 10))

			res := httptest.NewRecorder()
			server.ServeHTTP(res, req)

			if res.Code != tc.wantStatusCode {
				t.Fatalf("status=%d want=%d", res.Code, tc.wantStatusCode)
			}
			if deployer.calls != 1 {
				t.Fatalf("calls=%d want=1", deployer.calls)
			}
		})
	}
}

func sign(key string, payload []byte) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write(payload)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestWebhookArtifactDigestValidation(t *testing.T) {
	now := time.Now().Unix()
	newPayload := func(digest string) []byte {
		field := ""
		if digest != "" {
			field = `"artifact_digest":"` + digest + `",`
		}
		return []byte(`{"artifact_name":"release_2.4.7.tar.gz",` + field + `"timestamp":` + strconv.FormatInt(now, 10) + `}`)
	}

	tests := []struct {
		name           string
		allowMissing   bool
		digest         string
		wantStatusCode int
		wantCalls      int
	}{
		{name: "missing digest rejected by default", wantStatusCode: http.StatusBadRequest},
		{name: "missing digest allowed by config", allowMissing: true, wantStatusCode: http.StatusOK, wantCalls: 1},
		{name: "valid digest", digest: testDigest, wantStatusCode: http.StatusOK, wantCalls: 1},
		{name: "uppercase hex accepted", digest: "sha256:" + strings.ToUpper(strings.TrimPrefix(testDigest, "sha256:")), wantStatusCode: http.StatusOK, wantCalls: 1},
		{name: "missing algorithm prefix", digest: strings.TrimPrefix(testDigest, "sha256:"), wantStatusCode: http.StatusBadRequest},
		{name: "wrong algorithm", digest: "sha512:" + strings.TrimPrefix(testDigest, "sha256:"), wantStatusCode: http.StatusBadRequest},
		{name: "short hex", digest: "sha256:abcd", wantStatusCode: http.StatusBadRequest},
		{name: "invalid digest even when missing allowed", allowMissing: true, digest: "sha256:zz", wantStatusCode: http.StatusBadRequest},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{
				WebhookPath:                "/deploy",
				WebhookSecret:              "secret",
				HMACKey:                    "hmac",
				AllowMissingArtifactDigest: tc.allowMissing,
				Deploy: DeployConfig{
					DefaultBrand:  "demo",
					DefaultDomain: "shop.example",
				},
			}
			cfg.applyDefaults()
			deployer := &fakeDeployer{}
			server := &Server{cfg: cfg, deployer: deployer}
			payload := newPayload(tc.digest)

			req := httptest.NewRequest(http.MethodPost, "/deploy", bytes.NewReader(payload))
			req.Header.Set("X-Webhook-Secret", "secret")
			req.Header.Set("X-Signature", sign("hmac", payload))
			req.Header.Set("X-Timestamp", strconv.FormatInt(now, 10))
			res := httptest.NewRecorder()
			server.ServeHTTP(res, req)

			if res.Code != tc.wantStatusCode {
				t.Fatalf("status=%d want=%d body=%q", res.Code, tc.wantStatusCode, res.Body.String())
			}
			if deployer.calls != tc.wantCalls {
				t.Fatalf("calls=%d want=%d", deployer.calls, tc.wantCalls)
			}
		})
	}
}

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
	if err := verifyArtifactDigest("", actual); err != nil {
		t.Fatalf("empty expected: %v", err)
	}
	if err := verifyArtifactDigest("sha256:"+strings.Repeat("0", 64), actual); err == nil {
		t.Fatal("mismatch not detected")
	}
}
