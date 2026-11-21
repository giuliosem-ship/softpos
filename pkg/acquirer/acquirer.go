// Package acquirer is the provider-agnostic port to the certified card
// processor ("camino A", docs/ACQUIRER_INTEGRATION.md). Kerno never approves
// a card payment itself: the processor's Tap to Pay SDK runs on the phone,
// the processor authorizes and captures, and the processor's signed webhooks
// are the only source of truth for the final status and for every ledger
// movement.
//
// A real adapter (Adyen, Mercado Pago, Clip, ...) implements Provider and
// translates its webhook payloads into Event. MockProvider is the
// deterministic adapter used by tests and DEV_MODE.
package acquirer

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"time"
)

var (
	// ErrProviderUnavailable means the outcome of the call is unknown
	// (timeout, connection reset, 5xx). Callers retry with the same
	// idempotency key; they never assume success or failure.
	ErrProviderUnavailable = errors.New("acquirer provider unavailable (outcome unknown)")
	// ErrProviderRejected is a definitive rejection of the request.
	ErrProviderRejected = errors.New("acquirer provider rejected the request")
	// ErrPaymentNotFound is returned when the provider does not know the payment.
	ErrPaymentNotFound = errors.New("acquirer payment not found")
	// ErrRefundNotFound is returned by GetRefund when the provider never
	// received the refund request (no refund under that idempotency key).
	ErrRefundNotFound = errors.New("acquirer refund not found")
	// ErrInvalidEvent is returned when a webhook payload cannot be parsed or validated.
	ErrInvalidEvent = errors.New("invalid acquirer webhook event")
	// ErrUnknownProvider is returned by New for an unsupported provider name.
	ErrUnknownProvider = errors.New("unknown acquirer provider")
)

// Status is the provider-side state of a payment as reported by the port.
type Status string

const (
	StatusRequiresPayment Status = "requires_payment" // intent created, waiting for the tap
	StatusAuthorized      Status = "authorized"
	StatusCaptured        Status = "captured"
	StatusDeclined        Status = "declined"
	StatusCanceled        Status = "canceled"
	StatusRefunded        Status = "refunded"
)

// IntentRequest creates a payment intent at the provider.
type IntentRequest struct {
	// IdempotencyKey makes retries return the same intent (Kerno sends
	// "kerno_intent_<card_payment_id>").
	IdempotencyKey string
	MerchantID     string
	CardPaymentID  string // Kerno id, echoed back in webhooks as kerno_payment_id
	AmountCents    int64
	Currency       string // ISO 4217, "MXN"
	Reference      string
}

// Intent is what the phone needs to start the provider's Tap to Pay SDK.
type Intent struct {
	ProviderPaymentID string
	// ClientSecret is a short-lived, single-intent session token handed to the
	// provider SDK on the phone. It is never stored by Kerno.
	ClientSecret string
	Status       Status
	ExpiresAt    time.Time
}

// PaymentState is the provider's view of a payment.
type PaymentState struct {
	ProviderPaymentID string
	Status            Status
	AmountCents       int64
	CapturedCents     int64
	FeeCents          int64
}

// CaptureRequest captures an authorized payment (manual-capture providers).
type CaptureRequest struct {
	IdempotencyKey    string
	ProviderPaymentID string
	AmountCents       int64
}

// RefundRequest asks the provider to refund (part of) a captured payment.
type RefundRequest struct {
	IdempotencyKey    string // "kerno_refund_<refund_id>"
	ProviderPaymentID string
	AmountCents       int64
	// KernoRefundID is Kerno's refund id; adapters send it as metadata and
	// echo it in refund webhooks as Event.KernoRefundID.
	KernoRefundID string
}

// RefundResult is the provider's acknowledgement of a refund request. The
// ledger only moves when the refund.succeeded / payment.refunded webhook arrives.
type RefundResult struct {
	ProviderRefundID string
	Status           string // "pending" | "succeeded" | "failed"
}

// RefundLookup identifies a refund request at the provider.
type RefundLookup struct {
	ProviderPaymentID string
	IdempotencyKey    string // "kerno_refund_<refund_id>"
	KernoRefundID     string
}

// RefundState is the provider's view of a refund request.
type RefundState struct {
	ProviderRefundID string
	Status           string // "pending" | "succeeded" | "failed"
	AmountCents      int64
}

// Refund statuses reported by the provider.
const (
	RefundStatusPending   = "pending"
	RefundStatusSucceeded = "succeeded"
	RefundStatusFailed    = "failed"
)

// CancelRequest cancels an intent that was not captured.
type CancelRequest struct {
	IdempotencyKey    string
	ProviderPaymentID string
}

// OnboardRequest registers an approved merchant as a sub-merchant (sub-
// afiliado) at the processor, a prerequisite to accept its card payments.
type OnboardRequest struct {
	// IdempotencyKey makes retries return the same sub-merchant
	// ("kerno_onboard_<merchant_id>").
	IdempotencyKey string
	MerchantID     string
	LegalName      string
	RFC            string
	MCC            string
	PostalCode     string
	ContactEmail   string
	ContactPhone   string
}

// OnboardResult is the processor's sub-merchant account.
type OnboardResult struct {
	ProviderMerchantID string
	Status             string // "active" | "pending" (processor-side review)
}

// Provider is the port every acquirer adapter implements. Every mutating call
// carries an idempotency key; the provider must return the same result for a
// repeated key. Errors: ErrProviderUnavailable (unknown outcome, retry),
// ErrProviderRejected (definitive), ErrPaymentNotFound.
type Provider interface {
	// Name is the provider id used in config (ACQUIRER_PROVIDER), in the
	// webhook path /api/v1/webhooks/acquirer/{provider} and in the database.
	Name() string
	CreatePaymentIntent(ctx context.Context, req IntentRequest) (Intent, error)
	GetPayment(ctx context.Context, providerPaymentID string) (PaymentState, error)
	Capture(ctx context.Context, req CaptureRequest) (PaymentState, error)
	Refund(ctx context.Context, req RefundRequest) (RefundResult, error)
	// GetRefund looks a refund request up by the idempotency key Kerno sent
	// ("kerno_refund_<refund_id>", also echoed as kerno_refund_id) for the
	// refund sweep (MVP-29). ErrRefundNotFound means the provider never
	// received it; ErrProviderUnavailable means the outcome is still unknown.
	GetRefund(ctx context.Context, req RefundLookup) (RefundState, error)
	Cancel(ctx context.Context, req CancelRequest) (PaymentState, error)
	// OnboardMerchant creates (idempotently) the processor sub-merchant of an
	// approved Kerno merchant (called when an operator approves its KYB).
	OnboardMerchant(ctx context.Context, req OnboardRequest) (OnboardResult, error)
	// ParseWebhookEvent translates a verified webhook body into a normalized Event.
	ParseWebhookEvent(body []byte) (Event, error)
}

// WebhookVerifier is optionally implemented by adapters whose provider signs
// webhooks with its own scheme. Without it the server verifies the Kerno
// scheme (HMAC-SHA256 over "<timestamp>.<raw body>" with
// ACQUIRER_WEBHOOK_SECRET, headers X-Kerno-Timestamp / X-Kerno-Signature).
type WebhookVerifier interface {
	VerifyWebhook(header http.Header, body []byte, now time.Time) error
}

// EventType is a normalized webhook event type.
type EventType string

const (
	EventAuthorized         EventType = "payment.authorized"
	EventCaptured           EventType = "payment.captured"
	EventDeclined           EventType = "payment.declined"
	EventCanceled           EventType = "payment.canceled"
	EventRefunded           EventType = "payment.refunded"
	EventRefundFailed       EventType = "refund.failed"
	EventChargebackOpened   EventType = "chargeback.opened"
	EventChargebackReversed EventType = "chargeback.reversed"
	EventPayoutSettled      EventType = "payout.settled"
)

// PayoutItemKind classifies a line of a provider payout report.
type PayoutItemKind string

const (
	PayoutItemCapture            PayoutItemKind = "capture"             // + amount - fee
	PayoutItemRefund             PayoutItemKind = "refund"              // - amount
	PayoutItemChargeback         PayoutItemKind = "chargeback"          // - amount
	PayoutItemChargebackReversal PayoutItemKind = "chargeback_reversal" // + amount
)

// PayoutItem is one line of a provider payout (settlement) to Kerno.
type PayoutItem struct {
	ProviderPaymentID string         `json:"provider_payment_id"`
	Kind              PayoutItemKind `json:"kind"`
	AmountCents       int64          `json:"amount_cents"`
	FeeCents          int64          `json:"fee_cents,omitempty"`
}

// NetCents is the item's contribution to the payout amount.
func (it PayoutItem) NetCents() int64 {
	switch it.Kind {
	case PayoutItemCapture:
		return it.AmountCents - it.FeeCents
	case PayoutItemRefund, PayoutItemChargeback:
		return -it.AmountCents
	case PayoutItemChargebackReversal:
		return it.AmountCents
	}
	return 0
}

// Payout is a provider settlement of funds to Kerno's bank account (T+n).
type Payout struct {
	PayoutID       string       `json:"payout_id"`
	AmountCents    int64        `json:"amount_cents"`    // deposited amount
	SettlementDate string       `json:"settlement_date"` // YYYY-MM-DD
	Items          []PayoutItem `json:"items"`
}

// Event is a normalized, provider-independent webhook event (also the wire
// format of the mock provider and of DEV_MODE simulations).
type Event struct {
	// ID is the provider's event id: the exactly-once key.
	ID                string    `json:"id"`
	Type              EventType `json:"type"`
	ProviderPaymentID string    `json:"provider_payment_id,omitempty"`
	// KernoPaymentID echoes the card_payment_id sent at intent creation
	// (metadata); used only when the provider payment id is not yet stored.
	KernoPaymentID string `json:"kerno_payment_id,omitempty"`
	// AmountCents: captured amount (captured), this refund's amount
	// (refunded), disputed amount (chargeback.*).
	AmountCents int64 `json:"amount_cents,omitempty"`
	// FeeCents is the provider's processing fee (captured).
	FeeCents int64 `json:"fee_cents,omitempty"`
	// SettlementDate is the expected payout date of a capture (YYYY-MM-DD).
	SettlementDate string `json:"settlement_date,omitempty"`
	RefundID       string `json:"refund_id,omitempty"`
	// KernoRefundID echoes RefundRequest.KernoRefundID (metadata); used to
	// match a refund webhook that arrives before the provider refund id was
	// stored (provider call timed out or still in flight).
	KernoRefundID string `json:"kerno_refund_id,omitempty"`
	ChargebackID  string `json:"chargeback_id,omitempty"`
	DeclineCode   string `json:"decline_code,omitempty"`
	// Card details of an authorized / captured payment, for the sale
	// receipt: brand ("visa", "mastercard", "amex", …), the last 4 digits
	// of the PAN (never the full PAN) and the issuer's authorization code.
	CardBrand  string    `json:"card_brand,omitempty"`
	CardLast4  string    `json:"card_last4,omitempty"`
	AuthCode   string    `json:"auth_code,omitempty"`
	Payout     *Payout   `json:"payout,omitempty"`
	OccurredAt time.Time `json:"occurred_at,omitempty"`
}

var (
	idRe    = regexp.MustCompile(`^[A-Za-z0-9_\-:.]{1,128}$`)
	dateRe  = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	last4Re = regexp.MustCompile(`^[0-9]{4}$`)
	brandRe = regexp.MustCompile(`^[A-Za-z][A-Za-z_ ]{0,19}$`)
	authRe  = regexp.MustCompile(`^[A-Za-z0-9]{1,12}$`)
)

func validDate(s string) bool {
	if !dateRe.MatchString(s) {
		return false
	}
	_, err := time.Parse("2006-01-02", s)
	return err == nil
}

// Validate checks the fields each event type needs.
func (e Event) Validate() error {
	bad := func(msg string) error { return fmt.Errorf("%w: %s", ErrInvalidEvent, msg) }
	if !idRe.MatchString(e.ID) {
		return bad("id")
	}
	if e.Type == EventPayoutSettled {
		p := e.Payout
		if p == nil || !idRe.MatchString(p.PayoutID) || p.AmountCents < 0 || !validDate(p.SettlementDate) {
			return bad("payout")
		}
		for _, it := range p.Items {
			switch it.Kind {
			case PayoutItemCapture, PayoutItemRefund, PayoutItemChargeback, PayoutItemChargebackReversal:
			default:
				return bad("payout item kind")
			}
			if !idRe.MatchString(it.ProviderPaymentID) || it.AmountCents <= 0 || it.FeeCents < 0 {
				return bad("payout item")
			}
		}
		return nil
	}
	if !idRe.MatchString(e.ProviderPaymentID) {
		return bad("provider_payment_id")
	}
	if e.KernoPaymentID != "" && !idRe.MatchString(e.KernoPaymentID) {
		return bad("kerno_payment_id")
	}
	if (e.CardLast4 != "" && !last4Re.MatchString(e.CardLast4)) || (e.CardBrand != "" && !brandRe.MatchString(e.CardBrand)) ||
		(e.AuthCode != "" && !authRe.MatchString(e.AuthCode)) {
		return bad("card details (last 4 digits only, brand, auth code)")
	}
	switch e.Type {
	case EventAuthorized, EventDeclined, EventCanceled:
	case EventCaptured:
		if e.AmountCents <= 0 || e.FeeCents < 0 || e.FeeCents > e.AmountCents {
			return bad("captured amount/fee")
		}
		if e.SettlementDate != "" && !validDate(e.SettlementDate) {
			return bad("settlement_date")
		}
	case EventRefunded, EventRefundFailed:
		if !idRe.MatchString(e.RefundID) || (e.Type == EventRefunded && e.AmountCents <= 0) ||
			(e.KernoRefundID != "" && !idRe.MatchString(e.KernoRefundID)) {
			return bad("refund")
		}
	case EventChargebackOpened, EventChargebackReversed:
		if !idRe.MatchString(e.ChargebackID) || e.AmountCents <= 0 {
			return bad("chargeback")
		}
	default:
		return bad("type")
	}
	return nil
}

// New builds the adapter named by ACQUIRER_PROVIDER. Only "mock" exists
// until a processor is contracted (docs/ACQUIRER_INTEGRATION.md).
func New(name string) (Provider, error) {
	switch name {
	case MockProviderName:
		return NewMockProvider(MockConfig{}), nil
	default:
		return nil, fmt.Errorf("%w %q (supported: mock)", ErrUnknownProvider, name)
	}
}
