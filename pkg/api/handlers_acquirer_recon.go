package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"time"

	"softpos/pkg/acquirer"
	"softpos/pkg/auth"
	"softpos/pkg/ledger"
	"softpos/pkg/store"
)

// applyAcquirerPayout records a processor payout (settlement of funds to
// Kerno's bank account, T+n) against the captured card payments, in one
// transaction: journal acq_payout_<provider>_<payout> (Dr ASSET:BANK_OPERATING,
// Cr ASSET:ACQUIRER_RECEIVABLE for the deposited amount), each matched
// capture marked settled, the payout row and one reconciliation item per
// difference (missing payment, amount / fee mismatch, duplicate settlement,
// payout total mismatch). Differences are also logged as ERROR alerts.
func (s *Server) applyAcquirerPayout(ctx context.Context, provider string, e acquirer.Event) ([]byte, error) {
	p := e.Payout
	var out []byte
	var found []store.ReconciliationItem
	err := s.moneyStore().InTx(ctx, func(tx store.MoneyTx) error {
		out, found = nil, nil
		opKey := "acq:" + provider + ":evt:" + e.ID
		claimed, _, prior, err := tx.ClaimOperation(ctx, opKey)
		if err != nil {
			return err
		}
		if !claimed {
			out = prior
			return nil
		}
		res := AcquirerEventResult{EventID: e.ID, EventType: e.Type, Result: "ignored", PayoutID: p.PayoutID}
		fresh, err := claimSub(ctx, tx, "acq:"+provider+":payout:"+p.PayoutID)
		if err != nil {
			return err
		}
		if fresh {
			if found, err = s.reconcilePayout(ctx, tx, provider, p); err != nil {
				return err
			}
			res.Result, res.Discrepancies = "applied", len(found)
		}
		if out, err = json.Marshal(res); err != nil {
			return err
		}
		return tx.CompleteOperation(ctx, opKey, http.StatusOK, out)
	})
	if err != nil {
		return nil, err
	}
	for _, it := range found {
		slog.Error("ALERT acquirer reconciliation discrepancy", "provider", provider, "payout_id", it.PayoutID, "kind", it.Kind,
			"card_payment_id", it.CardPaymentID, "provider_payment_id", it.ProviderPaymentID,
			"expected_cents", it.ExpectedCents, "actual_cents", it.ActualCents, "detail", it.Detail)
	}
	return out, nil
}

func (s *Server) reconcilePayout(ctx context.Context, tx store.MoneyTx, provider string, p *acquirer.Payout) ([]store.ReconciliationItem, error) {
	items := append([]acquirer.PayoutItem(nil), p.Items...)
	// Card rows are locked in a stable order (then the ledger accounts).
	sort.SliceStable(items, func(i, j int) bool { return items[i].ProviderPaymentID < items[j].ProviderPaymentID })

	var diffs []store.ReconciliationItem
	flag := func(kind string, it acquirer.PayoutItem, cpID string, expected, actual int64, detail string) {
		diffs = append(diffs, store.ReconciliationItem{
			DedupeKey: fmt.Sprintf("payout:%s:%s:%s:%s:%s", provider, p.PayoutID, it.ProviderPaymentID, it.Kind, kind),
			Provider:  provider, PayoutID: p.PayoutID, CardPaymentID: cpID, ProviderPaymentID: it.ProviderPaymentID,
			Kind: kind, ExpectedCents: expected, ActualCents: actual, Detail: detail,
		})
	}
	now := nowUTC()
	var expected int64
	seen := map[string]bool{}
	for _, it := range items {
		expected += it.NetCents()
		cp, ok, err := tx.GetCardPaymentByProviderForUpdate(ctx, provider, it.ProviderPaymentID)
		if err != nil {
			return nil, err
		}
		if !ok || !cp.Status.Captured() {
			detail := "payout line for a payment Kerno does not know"
			if ok {
				detail = "payout line for a payment not captured in Kerno (status " + string(cp.Status) + ")"
			}
			flag(store.ReconMissingPayment, it, cp.ID, 0, it.AmountCents, detail)
			continue
		}
		switch it.Kind {
		case acquirer.PayoutItemCapture:
			if it.AmountCents != cp.CapturedCents {
				flag(store.ReconAmountMismatch, it, cp.ID, cp.CapturedCents, it.AmountCents, "captured amount differs")
			}
			if it.FeeCents != cp.ProviderFeeCents {
				flag(store.ReconFeeMismatch, it, cp.ID, cp.ProviderFeeCents, it.FeeCents, "processor fee differs from the captured event")
			}
			if seen[cp.ID] || (cp.ProviderPayoutID != "" && cp.ProviderPayoutID != p.PayoutID) {
				flag(store.ReconDuplicateSettlement, it, cp.ID, 0, it.AmountCents, "capture already settled in payout "+cp.ProviderPayoutID)
				continue
			}
			seen[cp.ID] = true
			cp.ProviderPayoutID, cp.ProviderSettledAt = p.PayoutID, &now
			if err := tx.UpdateCardPayment(ctx, cp); err != nil {
				return nil, err
			}
		case acquirer.PayoutItemRefund:
			if it.AmountCents > cp.RefundedCents {
				flag(store.ReconAmountMismatch, it, cp.ID, cp.RefundedCents, it.AmountCents, "refund deducted above the refunds recorded")
			}
		case acquirer.PayoutItemChargeback, acquirer.PayoutItemChargebackReversal:
			if cp.ChargebackCents == 0 && cp.Status != store.CardChargebackReversed {
				flag(store.ReconMissingPayment, it, cp.ID, 0, it.AmountCents, "chargeback line without a recorded chargeback")
			}
		}
	}
	if expected != p.AmountCents {
		diffs = append(diffs, store.ReconciliationItem{
			DedupeKey: fmt.Sprintf("payout:%s:%s:total", provider, p.PayoutID), Provider: provider, PayoutID: p.PayoutID,
			Kind: store.ReconPayoutTotalMismatch, ExpectedCents: expected, ActualCents: p.AmountCents,
			Detail: "deposited amount differs from the sum of the payout lines",
		})
	}

	journalID := "acq_payout_" + provider + "_" + p.PayoutID
	if p.AmountCents > 0 {
		jnl, err := ledger.NewJournalEntry(journalID, now, "Processor payout "+p.PayoutID, []ledger.Entry{
			{AccountID: AccountBankOperating, Direction: ledger.Debit, Money: ledger.NewMoney(p.AmountCents, "MXN")},
			{AccountID: AccountAcquirerReceivable, Direction: ledger.Credit, Money: ledger.NewMoney(p.AmountCents, "MXN")},
		}, map[string]string{"provider": provider, "payout_id": p.PayoutID, "settlement_date": p.SettlementDate})
		if err != nil {
			return nil, err
		}
		if err := tx.PostJournal(ctx, jnl); err != nil {
			return nil, err
		}
	}
	status := "RECONCILED"
	if len(diffs) > 0 {
		status = "DISCREPANCY"
	}
	if _, err := tx.InsertAcquirerPayout(ctx, store.AcquirerPayoutRecord{
		Provider: provider, PayoutID: p.PayoutID, AmountCents: p.AmountCents, ExpectedCents: expected,
		SettlementDate: p.SettlementDate, ItemCount: len(p.Items), DiscrepancyCount: len(diffs), Status: status, JournalID: journalID,
	}); err != nil {
		return nil, err
	}
	var stored []store.ReconciliationItem
	for _, d := range diffs {
		created, err := tx.InsertReconciliationItem(ctx, d)
		if err != nil {
			return nil, err
		}
		if created {
			stored = append(stored, d)
		}
	}
	return stored, tx.EnqueueEvent(ctx, "acquirer.payout_settled", provider+":"+p.PayoutID, map[string]any{
		"provider": provider, "payout_id": p.PayoutID, "amount_cents": p.AmountCents, "expected_cents": expected,
		"settlement_date": p.SettlementDate, "status": status, "discrepancies": len(stored),
	})
}

// ----------------------------------------------------------------------------
// Operator report
// ----------------------------------------------------------------------------

// ReconciliationItemResponse is one line of the processor reconciliation report.
type ReconciliationItemResponse struct {
	ID                int64  `json:"id"`
	Kind              string `json:"kind"`
	Provider          string `json:"provider"`
	PayoutID          string `json:"payout_id,omitempty"`
	CardPaymentID     string `json:"card_payment_id,omitempty"`
	ProviderPaymentID string `json:"provider_payment_id,omitempty"`
	ExpectedCents     int64  `json:"expected_cents"`
	ActualCents       int64  `json:"actual_cents"`
	Detail            string `json:"detail,omitempty"`
	Status            string `json:"status"`
	ResolutionNote    string `json:"resolution_note,omitempty"`
	ResolvedBy        string `json:"resolved_by,omitempty"`
	CreatedAt         string `json:"created_at"`
	ResolvedAt        string `json:"resolved_at,omitempty"`
}

// AcquirerPayoutResponse summarizes one processor payout.
type AcquirerPayoutResponse struct {
	Provider         string `json:"provider"`
	PayoutID         string `json:"payout_id"`
	AmountCents      int64  `json:"amount_cents"`
	ExpectedCents    int64  `json:"expected_cents"`
	SettlementDate   string `json:"settlement_date"`
	ItemCount        int    `json:"item_count"`
	DiscrepancyCount int    `json:"discrepancy_count"`
	Status           string `json:"status"`
	CreatedAt        string `json:"created_at"`
}

func (s *Server) acquirerRepo() (store.AcquirerRepository, bool) {
	repo, ok := s.moneyStore().(store.AcquirerRepository)
	return repo, ok
}

func requireOperator(w http.ResponseWriter, r *http.Request) bool {
	return requireRole(w, r, auth.RoleOperator)
}

// HandleAcquirerReconciliation: GET /api/v1/acquirer/reconciliation
// ?status=OPEN (default) | RESOLVED | ALL &limit=N (role operator).
func (s *Server) HandleAcquirerReconciliation(w http.ResponseWriter, r *http.Request) {
	if !requireOperator(w, r) {
		return
	}
	repo, ok := s.acquirerRepo()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "RECONCILIATION_UNAVAILABLE")
		return
	}
	status := r.URL.Query().Get("status")
	switch status {
	case "":
		status = "OPEN"
	case "ALL":
		status = ""
	case "OPEN", "RESOLVED":
	default:
		writeError(w, http.StatusBadRequest, "INVALID_STATUS")
		return
	}
	limit := 200
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 || n > 1000 {
			writeError(w, http.StatusBadRequest, "INVALID_LIMIT")
			return
		}
		limit = n
	}
	ctx := r.Context()
	items, err := repo.ListReconciliationItems(ctx, status, limit)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "RECONCILIATION_UNAVAILABLE")
		return
	}
	payoutRecs, err := repo.ListAcquirerPayouts(ctx, 50)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "RECONCILIATION_UNAVAILABLE")
		return
	}
	outItems := make([]ReconciliationItemResponse, 0, len(items))
	for _, it := range items {
		ri := ReconciliationItemResponse{
			ID: it.ID, Kind: it.Kind, Provider: it.Provider, PayoutID: it.PayoutID, CardPaymentID: it.CardPaymentID,
			ProviderPaymentID: it.ProviderPaymentID, ExpectedCents: it.ExpectedCents, ActualCents: it.ActualCents,
			Detail: it.Detail, Status: it.Status, ResolutionNote: it.ResolutionNote, ResolvedBy: it.ResolvedBy, CreatedAt: it.CreatedAt.UTC().Format(time.RFC3339),
		}
		if it.ResolvedAt != nil {
			ri.ResolvedAt = it.ResolvedAt.UTC().Format(time.RFC3339)
		}
		outItems = append(outItems, ri)
	}
	outPayouts := make([]AcquirerPayoutResponse, 0, len(payoutRecs))
	for _, p := range payoutRecs {
		outPayouts = append(outPayouts, AcquirerPayoutResponse{
			Provider: p.Provider, PayoutID: p.PayoutID, AmountCents: p.AmountCents, ExpectedCents: p.ExpectedCents,
			SettlementDate: p.SettlementDate, ItemCount: p.ItemCount, DiscrepancyCount: p.DiscrepancyCount, Status: p.Status,
			CreatedAt: p.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": outItems, "payouts": outPayouts})
}

// HandleResolveReconciliationItem: POST
// /api/v1/acquirer/reconciliation/{item}/resolve {"note"} (role operator).
func (s *Server) HandleResolveReconciliationItem(w http.ResponseWriter, r *http.Request) {
	if !requireOperator(w, r) {
		return
	}
	repo, ok := s.acquirerRepo()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "RECONCILIATION_UNAVAILABLE")
		return
	}
	id, err := strconv.ParseInt(r.PathValue("item"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "INVALID_ITEM_ID")
		return
	}
	var req struct {
		Note string `json:"note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Note == "" || len(req.Note) > 1000 {
		writeError(w, http.StatusBadRequest, "RESOLUTION_NOTE_REQUIRED")
		return
	}
	// Resolving moves no money; who closed the difference is part of the
	// report (audit trail), not only of the logs.
	user, _ := UserIDFromContext(r.Context())
	if user == "" {
		writeError(w, http.StatusForbidden, "OPERATOR_IDENTITY_REQUIRED")
		return
	}
	done, err := repo.ResolveReconciliationItem(r.Context(), id, req.Note, user)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "RECONCILIATION_UNAVAILABLE")
		return
	}
	if !done {
		writeError(w, http.StatusNotFound, "OPEN_ITEM_NOT_FOUND")
		return
	}
	slog.Info("acquirer reconciliation item resolved", "item_id", id, "operator", user)
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "status": "RESOLVED"})
}
