package api

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"softpos/pkg/acquirer"
	"softpos/pkg/auth"
	"softpos/pkg/store"
)

// lossyProvider loses the answer of Refund calls: with deliver=false the
// request never reaches the processor (crash before the call), with
// deliver=true the processor gets it but the response is lost (timeout).
type lossyProvider struct {
	*acquirer.MockProvider
	mu      sync.Mutex
	lose    bool
	deliver bool
}

func (p *lossyProvider) Refund(ctx context.Context, req acquirer.RefundRequest) (acquirer.RefundResult, error) {
	p.mu.Lock()
	lose, deliver := p.lose, p.deliver
	p.mu.Unlock()
	if !lose {
		return p.MockProvider.Refund(ctx, req)
	}
	if deliver {
		_, _ = p.MockProvider.Refund(ctx, req)
	}
	return acquirer.RefundResult{}, fmt.Errorf("%w: response lost", acquirer.ErrProviderUnavailable)
}

func setupSweepServer(t *testing.T, deliver bool) (*Server, *acquirer.MockProvider, *lossyProvider) {
	t.Helper()
	s, mock := setupWebhookCardServer(t)
	lp := &lossyProvider{MockProvider: mock, deliver: deliver}
	s.SetAcquirerProvider(lp)
	return s, mock, lp
}

func stuckRefund(t *testing.T, s *Server, mock *acquirer.MockProvider, lp *lossyProvider, key string, captured, amount int64) (CardPaymentIntentResponse, string) {
	t.Helper()
	intent := capturedCardPayment(t, s, mock, "cp-"+key, captured)
	lp.mu.Lock()
	lp.lose = true
	lp.mu.Unlock()
	code, _, body := requestRefund(t, s, auth.RoleOwner, intent.CardPaymentID, key, amount)
	lp.mu.Lock()
	lp.lose = false
	lp.mu.Unlock()
	if code != http.StatusBadGateway {
		t.Fatalf("lost refund: %d %s", code, body)
	}
	var id string
	_ = s.moneyStore().InTx(context.Background(), func(tx store.MoneyTx) error {
		rf, _, err := tx.GetCardRefundByKeyForUpdate(context.Background(), intent.CardPaymentID, key)
		id = rf.ID
		if rf.Status != store.RefundRequested {
			t.Fatalf("want REQUESTED, got %+v", rf)
		}
		return err
	})
	return intent, id
}

func refundStatus(t *testing.T, s *Server, id string) store.CardRefundRecord {
	t.Helper()
	var rf store.CardRefundRecord
	if err := s.moneyStore().InTx(context.Background(), func(tx store.MoneyTx) error {
		var err error
		rf, _, err = tx.GetCardRefundForUpdate(context.Background(), id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return rf
}

var sweepLater = time.Now().Add(time.Hour)

// TestRefundSweep_NeverReceivedExpiresAndReleasesHold: the provider never got
// the request; the sweep marks it FAILED (expired) and the amount is
// refundable again. Fresh REQUESTED refunds are not touched.
func TestRefundSweep_NeverReceivedExpiresAndReleasesHold(t *testing.T) {
	s, mock, lp := setupSweepServer(t, false)
	intent, id := stuckRefund(t, s, mock, lp, "lost-1", 20000, 20000)
	ctx := context.Background()
	if res, err := s.SweepStaleRefunds(ctx, time.Now(), RefundSweepConfig{}); err != nil || res.Claimed != 0 {
		t.Fatalf("a fresh REQUESTED refund must not be swept: %+v %v", res, err)
	}
	res, err := s.SweepStaleRefunds(ctx, sweepLater, RefundSweepConfig{})
	if err != nil || res.Claimed != 1 || res.Expired != 1 {
		t.Fatalf("sweep: %+v %v", res, err)
	}
	if rf := refundStatus(t, s, id); rf.Status != store.RefundFailed || rf.SweepAttempts != 1 {
		t.Fatalf("expired: %+v", rf)
	}
	if code, rf2, body := requestRefund(t, s, auth.RoleOwner, intent.CardPaymentID, "lost-2", 20000); code != http.StatusAccepted || rf2.Status != store.RefundPending {
		t.Fatalf("hold released: %d %s", code, body)
	}
	// The original key replays the expired request, no new provider call.
	calls := mock.Calls("Refund")
	if code, again, _ := requestRefund(t, s, auth.RoleOwner, intent.CardPaymentID, "lost-1", 20000); code != http.StatusAccepted || again.Status != store.RefundFailed || mock.Calls("Refund") != calls {
		t.Fatalf("replay of expired refund: %d %+v", code, again)
	}
}

// TestRefundSweep_ReceivedPendingThenWebhook: the provider got it (answer
// lost): the sweep stores PENDING + provider id; the webhook posts it once.
func TestRefundSweep_ReceivedPendingThenWebhook(t *testing.T) {
	s, mock, lp := setupSweepServer(t, true)
	intent, id := stuckRefund(t, s, mock, lp, "rp-1", 20000, 7000)
	res, err := s.SweepStaleRefunds(context.Background(), sweepLater, RefundSweepConfig{})
	if err != nil || res.Pending != 1 {
		t.Fatalf("sweep: %+v %v", res, err)
	}
	rf := refundStatus(t, s, id)
	if rf.Status != store.RefundPending || rf.ProviderRefundID == "" {
		t.Fatalf("pending: %+v", rf)
	}
	avail := "LIABILITY:MERCHANT:" + cardMerchant + ":AVAILABLE"
	before := balanceOf(t, s, avail)
	if code, body := deliver(t, s, acquirer.Event{ID: "evt_rp1", Type: acquirer.EventRefunded, ProviderPaymentID: intent.ProviderPaymentID, RefundID: rf.ProviderRefundID, AmountCents: 7000}); code != http.StatusOK {
		t.Fatalf("webhook: %d %s", code, body)
	}
	if balanceOf(t, s, avail) >= before || refundStatus(t, s, id).Status != store.RefundSucceeded {
		t.Fatal("webhook must apply the refund")
	}
}

// TestRefundSweep_SucceededAppliedOnceDespiteLateWebhook: the provider says
// succeeded; the sweep applies it through the webhook path and the real
// webhook arriving afterwards does not post it again.
func TestRefundSweep_SucceededAppliedOnceDespiteLateWebhook(t *testing.T) {
	s, mock, lp := setupSweepServer(t, true)
	intent, id := stuckRefund(t, s, mock, lp, "rs-1", 20000, 6000)
	if !mock.SetRefundStatus("kerno_refund_"+id, acquirer.RefundStatusSucceeded) {
		t.Fatal("mock must have received the refund")
	}
	avail := "LIABILITY:MERCHANT:" + cardMerchant + ":AVAILABLE"
	before := balanceOf(t, s, avail)
	res, err := s.SweepStaleRefunds(context.Background(), sweepLater, RefundSweepConfig{})
	if err != nil || res.Succeeded != 1 {
		t.Fatalf("sweep: %+v %v", res, err)
	}
	after := balanceOf(t, s, avail)
	rf := refundStatus(t, s, id)
	if after >= before || rf.Status != store.RefundSucceeded {
		t.Fatalf("applied: before=%d after=%d %+v", before, after, rf)
	}
	if code, body := deliver(t, s, acquirer.Event{ID: "evt_rs1_late", Type: acquirer.EventRefunded, ProviderPaymentID: intent.ProviderPaymentID, RefundID: rf.ProviderRefundID, AmountCents: 6000}); code != http.StatusOK {
		t.Fatalf("late webhook: %d %s", code, body)
	}
	if balanceOf(t, s, avail) != after {
		t.Fatal("late webhook must not post the refund twice")
	}
	if res, _ := s.SweepStaleRefunds(context.Background(), sweepLater.Add(time.Hour), RefundSweepConfig{}); res.Claimed != 0 {
		t.Fatalf("resolved refunds are not swept again: %+v", res)
	}
}

// TestRefundSweep_ProviderFailedReleasesHold: the provider reports failed.
func TestRefundSweep_ProviderFailedReleasesHold(t *testing.T) {
	s, mock, lp := setupSweepServer(t, true)
	_, id := stuckRefund(t, s, mock, lp, "rf-1", 20000, 5000)
	mock.SetRefundStatus("kerno_refund_"+id, acquirer.RefundStatusFailed)
	if res, err := s.SweepStaleRefunds(context.Background(), sweepLater, RefundSweepConfig{}); err != nil || res.Failed != 1 {
		t.Fatalf("sweep: %+v %v", res, err)
	}
	if rf := refundStatus(t, s, id); rf.Status != store.RefundFailed {
		t.Fatalf("failed: %+v", rf)
	}
}

// TestRefundSweep_UnknownAlertsAfterNAttempts: the provider stays
// unavailable; the refund stays REQUESTED and alerts after N attempts.
func TestRefundSweep_UnknownAlertsAfterNAttempts(t *testing.T) {
	s, mock, lp := setupSweepServer(t, true)
	amount := int64(1000 + acquirer.MockTimeoutSuffix) // the mock times out every call for ...52
	_, id := stuckRefund(t, s, mock, lp, "ru-1", 20000, amount)
	cfg := RefundSweepConfig{AlertAttempts: 3}
	now := sweepLater
	for i := 1; i <= 3; i++ {
		res, err := s.SweepStaleRefunds(context.Background(), now, cfg)
		if err != nil || res.Unknown != 1 || (i < 3 && res.Alerted != 0) || (i == 3 && res.Alerted != 1) {
			t.Fatalf("attempt %d: %+v %v", i, res, err)
		}
		now = now.Add(time.Hour)
	}
	if rf := refundStatus(t, s, id); rf.Status != store.RefundRequested || rf.SweepAttempts != 3 {
		t.Fatalf("still requested: %+v", rf)
	}
}

// TestRefundSweep_RaceWithWebhook: the sweep (provider says succeeded) and
// the webhook run concurrently; the ledger moves exactly once.
func TestRefundSweep_RaceWithWebhook(t *testing.T) {
	s, mock, lp := setupSweepServer(t, true)
	intent, id := stuckRefund(t, s, mock, lp, "rr-1", 20000, 4000)
	mock.SetRefundStatus("kerno_refund_"+id, acquirer.RefundStatusSucceeded)
	st, _ := mock.GetRefund(context.Background(), acquirer.RefundLookup{IdempotencyKey: "kerno_refund_" + id})
	avail := "LIABILITY:MERCHANT:" + cardMerchant + ":AVAILABLE"
	before := balanceOf(t, s, avail)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = s.SweepStaleRefunds(context.Background(), sweepLater, RefundSweepConfig{})
	}()
	go func() {
		defer wg.Done()
		_, _ = deliver(t, s, acquirer.Event{ID: "evt_rr1", Type: acquirer.EventRefunded, ProviderPaymentID: intent.ProviderPaymentID,
			RefundID: st.ProviderRefundID, KernoRefundID: id, AmountCents: 4000})
	}()
	wg.Wait()
	after := balanceOf(t, s, avail)
	if rf := refundStatus(t, s, id); rf.Status != store.RefundSucceeded {
		t.Fatalf("status: %+v", rf)
	}
	// Merchant available drops by the refunded net amount exactly once: a
	// second application would double it.
	single := before - after
	if single <= 0 {
		t.Fatalf("not applied: %d -> %d", before, after)
	}
	if code, _ := deliver(t, s, acquirer.Event{ID: "evt_rr1_again", Type: acquirer.EventRefunded, ProviderPaymentID: intent.ProviderPaymentID,
		RefundID: st.ProviderRefundID, AmountCents: 4000}); code != http.StatusOK || balanceOf(t, s, avail) != after {
		t.Fatal("refund applied twice")
	}
}

// retryRaceProvider lets a test run a merchant retry while the sweep is
// between its provider lookup and its resolution.
type retryRaceProvider struct {
	*lossyProvider
	onGetRefund func()
	refundGate  chan struct{} // Refund blocks until closed (when set)
	inRefund    chan struct{}
}

func (p *retryRaceProvider) GetRefund(ctx context.Context, req acquirer.RefundLookup) (acquirer.RefundState, error) {
	st, err := p.lossyProvider.GetRefund(ctx, req)
	if p.onGetRefund != nil {
		f := p.onGetRefund
		p.onGetRefund = nil
		f()
	}
	return st, err
}

func (p *retryRaceProvider) Refund(ctx context.Context, req acquirer.RefundRequest) (acquirer.RefundResult, error) {
	if p.refundGate != nil {
		close(p.inRefund)
		<-p.refundGate
	}
	return p.lossyProvider.Refund(ctx, req)
}

// TestRefundSweep_MerchantRetryDuringSweepIsNotExpired (MVP-29 audit): the
// sweep looks a never-received refund up (not found) while the merchant
// retries it with the same Idempotency-Key, which re-sends it to the
// provider. The sweep must not expire it (releasing the hold and answering
// the retry ACQUIRER_REJECTED) while the provider is processing it.
func TestRefundSweep_MerchantRetryDuringSweepIsNotExpired(t *testing.T) {
	s, mock, lp := setupSweepServer(t, false)
	intent, id := stuckRefund(t, s, mock, lp, "retry-1", 20000, 8000)
	rp := &retryRaceProvider{lossyProvider: lp}
	s.SetAcquirerProvider(rp)
	retried := make(chan int, 1)
	rp.onGetRefund = func() {
		rp.refundGate, rp.inRefund = make(chan struct{}), make(chan struct{})
		go func() {
			code, _, _ := requestRefund(t, s, auth.RoleOwner, intent.CardPaymentID, "retry-1", 8000)
			retried <- code
		}()
		<-rp.inRefund // the retry's provider call is in flight
	}
	res, err := s.SweepStaleRefunds(context.Background(), sweepLater, RefundSweepConfig{})
	if err != nil {
		t.Fatal(err)
	}
	close(rp.refundGate)
	if code := <-retried; code != http.StatusAccepted {
		t.Fatalf("the retry reached the provider and must be accepted, got %d (sweep %+v)", code, res)
	}
	if res.Expired != 0 {
		t.Fatalf("a refund retried during the sweep must not be expired: %+v", res)
	}
	if rf := refundStatus(t, s, id); rf.Status != store.RefundPending || rf.ProviderRefundID == "" {
		t.Fatalf("want PENDING with the provider id, got %+v", rf)
	}
}
