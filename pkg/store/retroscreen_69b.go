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
	Checked   int
	Blocked   []string // merchant ids blocked because their RFC is now LISTED
	Unblocked []string // 69-B blocks lifted: the RFC is no longer PRESUNTO/DEFINITIVO
	Excepted  []string // listed, not blocked: documented operator exception for that situation
	Unknown   int      // lookups that were not conclusive
}

// ReasonRetroscreen69B is the block reason set when an approved merchant's
// RFC appears in the SAT 69-B list (PRESUNTO / DEFINITIVO).
const ReasonRetroscreen69B = "RFC_LISTED_69B"

// Outbox events of the 69-B lifecycle (notify the merchant; MVP-27).
const (
	EventMerchantBlocked69B   = "merchant.blocked_69b"
	EventMerchantUnblocked69B = "merchant.unblocked_69b"
)

// Retroscreen69B re-checks every APPROVED merchant against the loaded list
// (docs/KYB.md "Ciclo de vida del bloqueo 69-B"):
//   - LISTED and not blocked: blocked (source SAT_69B) with an outbox event
//     in the same transaction and an audit row, unless an operator recorded
//     an exception for the same RFC and situation; a situation change
//     revokes older exceptions and re-blocks.
//   - no longer LISTED (DESVIRTUADO / SENTENCIA FAVORABLE / absent from a
//     current list) and blocked by the 69-B process: unblocked with an event
//     and an audit row. Blocks placed by an operator are never lifted here.
//   - an inconclusive lookup (stale / unavailable list) changes nothing.
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
		if class == kyb.List69BUnknown {
			res.Unknown++
			continue
		}
		before := rec
		var ev *KYBEvent
		action := ""
		switch {
		case class == kyb.List69BListed && !rec.Blocked:
			excepted, xerr := repo.ActiveSat69BException(ctx, rec.MerchantID, rfc, string(st))
			if xerr != nil {
				res.Unknown++
				continue
			}
			if excepted {
				res.Excepted = append(res.Excepted, rec.MerchantID)
				if rec.List69B == class {
					continue
				}
				rec.List69B = class
				break
			}
			now := time.Now().UTC()
			rec.List69B = class
			rec.Blocked, rec.BlockedReason, rec.BlockedAt, rec.BlockedSource = true, ReasonRetroscreen69B, &now, BlockSourceSAT69B
			action = "kyb.retroscreen_69b_block"
			ev = &KYBEvent{Type: EventMerchantBlocked69B, AggregateID: rec.MerchantID, Payload: map[string]any{
				"merchant_id": rec.MerchantID, "situation": string(st), "blocked_reason": ReasonRetroscreen69B, "blocked_at": now.Format(time.RFC3339)}}
		case class == kyb.List69BClear && rec.Blocked && rec.BlockedSource == BlockSourceSAT69B:
			now := time.Now().UTC()
			rec.List69B = class
			rec.Blocked, rec.BlockedReason, rec.BlockedAt, rec.BlockedSource = false, "", nil, ""
			action = "kyb.retroscreen_69b_unblock"
			ev = &KYBEvent{Type: EventMerchantUnblocked69B, AggregateID: rec.MerchantID, Payload: map[string]any{
				"merchant_id": rec.MerchantID, "situation": string(st), "unblocked_at": now.Format(time.RFC3339)}}
		default:
			if class == rec.List69B {
				continue
			}
			rec.List69B = class
		}
		var updated KYBRecord
		var uerr error
		if ev != nil {
			updated, uerr = repo.UpdateKYBWithEvent(ctx, rec, *ev)
		} else {
			updated, uerr = repo.UpdateKYB(ctx, rec)
		}
		if errors.Is(uerr, ErrKYBConflict) {
			res.Unknown++ // changed concurrently: the next run re-checks it
			continue
		}
		if uerr != nil {
			return res, fmt.Errorf("update kyb %s: %w", rec.MerchantID, uerr)
		}
		// Exceptions only cover the current situation: a block (situation
		// changed) or leaving the list revokes the rest.
		switch {
		case action == "kyb.retroscreen_69b_block" || class == kyb.List69BClear:
			if _, rerr := repo.RevokeSat69BExceptions(ctx, rec.MerchantID, "", actor); rerr != nil {
				slog.Error("69-B exceptions not revoked", "merchant_id", rec.MerchantID, "error", rerr)
			}
		}
		if action == "" {
			continue
		}
		if action == "kyb.retroscreen_69b_block" {
			res.Blocked = append(res.Blocked, rec.MerchantID)
			slog.Error("ALERT approved merchant blocked: RFC listed in SAT 69-B", "merchant_id", rec.MerchantID, "status", st)
		} else {
			res.Unblocked = append(res.Unblocked, rec.MerchantID)
			slog.Warn("approved merchant unblocked: RFC no longer listed in SAT 69-B", "merchant_id", rec.MerchantID, "status", st)
		}
		if audit != nil {
			if aerr := audit.AppendAudit(ctx, AuditEntry{OperatorID: actor, Action: action, MerchantID: rec.MerchantID,
				Before: auditJSON(map[string]any{"blocked": before.Blocked, "blocked_source": before.BlockedSource, "list_69b": before.List69B}),
				After:  auditJSON(map[string]any{"blocked": updated.Blocked, "blocked_source": updated.BlockedSource, "list_69b": updated.List69B}),
				Detail: "SAT 69-B status " + string(st)}); aerr != nil {
				slog.Error("ALERT operator audit write failed", "action", action, "merchant_id", rec.MerchantID, "error", aerr)
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
