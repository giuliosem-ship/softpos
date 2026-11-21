package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"softpos/pkg/identity"
)

// TestStartSAT69BWorker_DownloadsAtBoot: with SAT_69B_URL the worker loads
// the list at boot (here from a local HTTP server) and KYB lookups stop
// being UNKNOWN; without it the worker is a no-op.
func TestStartSAT69BWorker_DownloadsAtBoot(t *testing.T) {
	csv, err := os.ReadFile(filepath.Join("..", "..", "pkg", "identity", "testdata", "sat69b_latin1.csv"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(csv) }))
	defer srv.Close()

	cfg := Config{DevMode: true, SAT69BURL: srv.URL + "/Listado_Completo_69-B.csv", SAT69BRefreshInterval: time.Hour}
	server, _, _, err := BuildServer(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	wait := StartSAT69BWorker(ctx, cfg, server)
	deadline := time.Now().Add(5 * time.Second)
	var st identity.BlacklistStatus
	for time.Now().Before(deadline) {
		if st, err = server.Blacklist().Lookup(context.Background(), "AAA120730823"); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	wait()
	if st != identity.BlacklistDefinitivo {
		t.Fatalf("worker did not load the list: %q %v", st, err)
	}
	StartSAT69BWorker(context.Background(), Config{}, server)() // no URL: returns immediately
}
