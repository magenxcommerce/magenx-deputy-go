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
	"testing"
	"time"
)

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
	payload := []byte(fmt.Sprintf(`{"artifact_name":"release_2.4.7.tar.gz","brand":"demo","domain":"shop.example","timestamp":%d}`, now))

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
	payload := []byte(`{"artifact_name":"release_2.4.7.tar.gz","brand":"demo","domain":"shop.example","timestamp":` + strconv.FormatInt(now, 10) + `}`)

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
