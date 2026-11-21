package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"softpos/pkg/identity"
	"softpos/pkg/kyb"
)

// Retroscreen69BResult summarizes a re-check of approved merchants after a
// new SAT 69-B list was loaded.
type Retroscreen69BResult struct {
	Checked int
	Blocked []string // merchant ids blocked because their RFC is now LISTED
	Unknown int      // lookups that were not conclusive
}

// ReasonRetroscreen69B is the block reason set when an approved merchant's
// RFC appears in the SAT 69-B list (PRESUNTO / DEFINITIVO).
const ReasonRetroscreen69B = "RFC_LISTED_69B"

// Retroscreen69B re-checks every APPROVED merchant against the loaded list:
// a LISTED RFC blocks the merchant (money routes answer 403 MERCHANT_BLOCKED)
// with an operator_audit_log row by actor; the operator reviews and may
// unblock. list_69b is refreshed for the others.
func Retroscreen69B(ctx context.Context, repo KYBRepository, audit OperatorAuditRepository, lookup func(ctx context.Context, rfc string) (identity.BlacklistStatus, error), actor string) (Retroscreen69BResult, error) {
	var res Retroscreen69BResult
	recs, err := repo.ListKYB(ctx, KYBQuery{Status: kyb.StatusApproved, Limit: maxKYBListing})
	if err != nil {
		return res, fmt.Errorf("list approved merchants: %w", err)
	}
	for _, rec := range recs {
		rfc := rec.Application.Normalize().RFC
		if rfc == "" {
			continue
		}
		res.Checked++
		st, lerr := lookup(ctx, rfc)
		if lerr != nil {
			res.Unknown++
			continue
		}
		class := kyb.Classify69B(st)
		if class == rec.List69B && (class != kyb.List69BListed || rec.Blocked) {
			continue
		}
		before := rec
		rec.List69B = class
		blocking := class == kyb.List69BListed && !rec.Blocked
		if blocking {
			now := time.Now().UTC()
			rec.Blocked, rec.BlockedReason, rec.BlockedAt = true, ReasonRetroscreen69B, &now
		}
		updated, uerr := repo.UpdateKYB(ctx, rec)
		if errors.Is(uerr, ErrKYBConflict) {
			res.Unknown++ // changed concurrently: the next run re-checks it
			continue
		}
		if uerr != nil {
			return res, fmt.Errorf("update kyb %s: %w", rec.MerchantID, uerr)
		}
		if blocking {
			res.Blocked = append(res.Blocked, rec.MerchantID)
			slog.Error("ALERT approved merchant blocked: RFC listed in SAT 69-B", "merchant_id", rec.MerchantID, "status", st)
			if audit != nil {
				if aerr := audit.AppendAudit(ctx, AuditEntry{OperatorID: actor, Action: "kyb.retroscreen_69b_block", MerchantID: rec.MerchantID,
					Before: auditJSON(map[string]any{"blocked": before.Blocked, "list_69b": before.List69B}),
					After:  auditJSON(map[string]any{"blocked": updated.Blocked, "list_69b": updated.List69B}),
					Detail: "SAT 69-B status " + string(st)}); aerr != nil {
					slog.Error("ALERT operator audit write failed", "action", "kyb.retroscreen_69b_block", "merchant_id", rec.MerchantID, "error", aerr)
				}
			}
		}
	}
	return res, nil
}

func auditJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}
