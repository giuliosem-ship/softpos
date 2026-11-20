// Package payouts holds the payout (SPEI disbursement) ledger postings shared
// by the HTTP handler and the recovery reconciler, and the reconciler that
// resolves payouts whose STP outcome is unknown or that were interrupted
// between the funds reservation and registraOrden.
package payouts

import (
	"fmt"
	"time"

	"softpos/pkg/ledger"
	"softpos/pkg/store"
)

// ConcentratorAccount is the platform's STP concentrator bank account.
const ConcentratorAccount = "ASSET:STP_CONCENTRATOR"

// AvailableAccount is the merchant's available-balance liability account.
func AvailableAccount(merchantID string) string {
	return fmt.Sprintf("LIABILITY:MERCHANT:%s:AVAILABLE", merchantID)
}

// ReservationJournalID / CompensationJournalID are deterministic per payout,
// so a second compensation attempt fails with store.ErrDuplicateJournal
// instead of returning the funds twice.
func ReservationJournalID(claveRastreo string) string  { return "disb_" + claveRastreo }
func CompensationJournalID(claveRastreo string) string { return "disb_rev_" + claveRastreo }

// ReservationJournal debits the merchant's available balance against the
// concentrator (funds leave when the SPEI order is sent).
func ReservationJournal(p store.PayoutRecord, at time.Time) (*ledger.JournalEntry, error) {
	return payoutJournal(p, at, ReservationJournalID(p.ClaveRastreo), "Disbursement SPEI to "+p.DestinationCLABE, false)
}

// CompensationJournal returns a reserved payout's funds to the merchant.
func CompensationJournal(p store.PayoutRecord, at time.Time, reason string) (*ledger.JournalEntry, error) {
	return payoutJournal(p, at, CompensationJournalID(p.ClaveRastreo), "Disbursement "+reason+" "+p.ClaveRastreo, true)
}

func payoutJournal(p store.PayoutRecord, at time.Time, id, description string, reverse bool) (*ledger.JournalEntry, error) {
	avail := ledger.Entry{AccountID: AvailableAccount(p.MerchantID), Direction: ledger.Debit, Money: ledger.NewMoney(p.AmountCents, "MXN")}
	conc := ledger.Entry{AccountID: ConcentratorAccount, Direction: ledger.Credit, Money: ledger.NewMoney(p.AmountCents, "MXN")}
	if reverse {
		avail.Direction, conc.Direction = ledger.Credit, ledger.Debit
	}
	return ledger.NewJournalEntry(id, at, description, []ledger.Entry{avail, conc},
		map[string]string{"clave_rastreo": p.ClaveRastreo, "merchant_id": p.MerchantID})
}
