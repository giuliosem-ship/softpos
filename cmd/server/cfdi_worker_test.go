package main

import (
	"context"
	"testing"
	"time"
)

// TestStartCFDIWorkers_RunAndStop: the factura global worker and the
// cancellation poller start, run once and stop with the context.
func TestStartCFDIWorkers_RunAndStop(t *testing.T) {
	server, _, _, err := BuildServer(context.Background(), Config{DevMode: true, SeedDemoMerchant: true})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	wait := StartCFDIWorkers(ctx, Config{CFDIGlobalInvoiceInterval: 10 * time.Millisecond, CFDICancelPollInterval: 10 * time.Millisecond, CFDICreditNoteInterval: 10 * time.Millisecond}, server)
	time.Sleep(50 * time.Millisecond)
	cancel()
	done := make(chan struct{})
	go func() { wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("CFDI workers did not stop")
	}
}
