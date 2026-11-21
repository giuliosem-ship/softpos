package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// TestStartMetrics_ServesWithToken (MVP-30): /metrics is served on its own
// listener with the bearer token and stops with the context.
func TestStartMetrics_ServesWithToken(t *testing.T) {
	if wait, err := StartMetrics(context.Background(), Config{}, nil); err != nil {
		t.Fatal(err)
	} else {
		wait() // no METRICS_ADDR: nothing started
	}
	addr := freeAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	wait, err := StartMetrics(ctx, Config{MetricsAddr: addr, MetricsToken: "metrics_token_with_at_least_32_bytes!!"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	get := func(token string) (int, string) {
		var resp *http.Response
		var err error
		for i := 0; i < 50; i++ {
			req, _ := http.NewRequest(http.MethodGet, "http://"+addr+"/metrics", nil)
			if token != "" {
				req.Header.Set("Authorization", "Bearer "+token)
			}
			if resp, err = http.DefaultClient.Do(req); err == nil {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, _ := get(""); code != http.StatusUnauthorized {
		t.Fatalf("without token: %d", code)
	}
	code, body := get("metrics_token_with_at_least_32_bytes!!")
	if code != http.StatusOK || !strings.Contains(body, "# TYPE kerno_http_requests_total counter") {
		t.Fatalf("metrics: %d %s", code, body)
	}
	cancel()
	done := make(chan struct{})
	go func() { wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("metrics listener did not stop")
	}
}

func TestValidateProductionConfig_MetricsToken(t *testing.T) {
	good := Config{
		DatabaseURL:        "postgres://softpos:Str0ngRandomPassw0rd@db.prod.internal:5432/softpos?sslmode=require",
		VaultMasterKeyHex:  "a1b2c3d4e5f67890123456789abcdef0123456789abcdef0123456789abcdef0",
		STPHMACSecret:      "prod_stp_hmac_secret_very_long_and_random_string_xyz",
		JWTSecret:          "prod_jwt_secret_with_at_least_32_random_bytes",
		CoDiWebhookSecret:  "prod_codi_webhook_secret_with_32_random_bytes",
		ReceiptTokenSecret: "prod_receipt_token_secret_32_random_bytes!",
		MetricsAddr:        "127.0.0.1:9090",
	}
	if err := ValidateProductionConfig(good, "production"); err != nil {
		t.Fatalf("loopback without token: %v", err)
	}
	good.MetricsAddr = "0.0.0.0:9090"
	if err := ValidateProductionConfig(good, "production"); err != ErrInsecureMetricsToken {
		t.Fatalf("non-loopback without token: %v", err)
	}
	good.MetricsToken = "prod_metrics_token_with_32_random_bytes"
	if err := ValidateProductionConfig(good, "production"); err != nil {
		t.Fatalf("with token: %v", err)
	}
}
