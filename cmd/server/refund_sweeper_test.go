package main

import (
	"context"
	"testing"
	"time"
)

// TestStartRefundSweeper_StopsOnCancel: the sweeper runs only with a card
// processor and exits when its context is cancelled.
func TestStartRefundSweeper_StopsOnCancel(t *testing.T) {
	plain, _, _, err := BuildServer(context.Background(), Config{STPHMACSecret: "test_secret", JWTSecret: testJWTSecret, VaultMasterKeyHex: testVaultKeyHex})
	if err != nil {
		t.Fatal(err)
	}
	StartRefundSweeper(context.Background(), Config{}, plain)() // no provider: returns at once
	dev, _, _, err := BuildServer(context.Background(), Config{DevMode: true})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	wait := StartRefundSweeper(ctx, Config{RefundSweepInterval: 10 * time.Millisecond, RefundSweepAfter: time.Second}, dev)
	time.Sleep(30 * time.Millisecond)
	cancel()
	done := make(chan struct{})
	go func() { wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("sweeper did not stop")
	}
}
