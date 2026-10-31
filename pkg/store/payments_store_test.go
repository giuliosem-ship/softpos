package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"softpos/pkg/payments"
)

func TestSQLEMVPaymentRepository_SaveAndGetPayment(t *testing.T) {
	db, cleanup := setupTestDB(t, "test_payments_save_get")
	defer cleanup()

	repo := NewSQLEMVPaymentRepository(db)
	ctx := context.Background()

	now := time.Now().UTC()
	record := EMVPaymentRecord{
		PaymentID:      "emv_tx_1001",
		MerchantID:     "merch_store_01",
		AmountCents:    116000, // $1,160.00 MXN
		Currency:       "MXN",
		Status:         payments.PaymentAuthorized,
		MaskedPAN:      "415231******9012",
		CryptogramType: payments.CryptogramARQC,
		CryptogramHex:  "A1B2C3D4E5F67890",
		ATC:            14,
		TLVPayloadHex:  "9F02060000001160005F2A0204849F2608A1B2C3D4E5F678909F270180",
		CreatedAt:      now,
		UpdatedAt:      now,
	}

	if err := repo.SavePayment(ctx, record); err != nil {
		t.Fatalf("SavePayment failed: %v", err)
	}

	fetched, found, err := repo.GetPayment(ctx, "emv_tx_1001")
	if err != nil {
		t.Fatalf("GetPayment failed: %v", err)
	}
	if !found || fetched == nil {
		t.Fatalf("expected payment to be found")
	}

	if fetched.PaymentID != "emv_tx_1001" || fetched.AmountCents != 116000 || fetched.Status != payments.PaymentAuthorized {
		t.Errorf("unexpected payment record: %+v", fetched)
	}
}

func TestSQLEMVPaymentRepository_StateTransitions(t *testing.T) {
	db, cleanup := setupTestDB(t, "test_payments_transitions")
	defer cleanup()

	repo := NewSQLEMVPaymentRepository(db)
	ctx := context.Background()

	now := time.Now().UTC()
	record := EMVPaymentRecord{
		PaymentID:      "emv_tx_1002",
		MerchantID:     "merch_store_01",
		AmountCents:    250000,
		Currency:       "MXN",
		Status:         payments.PaymentAuthorized,
		MaskedPAN:      "415231******9012",
		CryptogramType: payments.CryptogramARQC,
		CryptogramHex:  "A1B2C3D4E5F67890",
		ATC:            15,
		TLVPayloadHex:  "9F0206000000250000",
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := repo.SavePayment(ctx, record); err != nil {
		t.Fatalf("SavePayment failed: %v", err)
	}

	// 1. Valid transition: AUTHORIZED -> SETTLED
	if err := repo.TransitionStatus(ctx, "emv_tx_1002", payments.PaymentAuthorized, payments.PaymentSettled); err != nil {
		t.Fatalf("TransitionStatus AUTHORIZED -> SETTLED failed: %v", err)
	}

	fetched, _, _ := repo.GetPayment(ctx, "emv_tx_1002")
	if fetched.Status != payments.PaymentSettled {
		t.Errorf("expected status SETTLED, got: %s", fetched.Status)
	}

	// 2. Invalid transition: cannot transition from AUTHORIZED (since it's already SETTLED)
	err := repo.TransitionStatus(ctx, "emv_tx_1002", payments.PaymentAuthorized, payments.PaymentVoided)
	if !errors.Is(err, payments.ErrInvalidStateTransition) {
		t.Errorf("expected ErrInvalidStateTransition, got: %v", err)
	}

	// 3. Valid transition: SETTLED -> REFUNDED
	if err := repo.TransitionStatus(ctx, "emv_tx_1002", payments.PaymentSettled, payments.PaymentRefunded); err != nil {
		t.Fatalf("TransitionStatus SETTLED -> REFUNDED failed: %v", err)
	}

	fetchedRefunded, _, _ := repo.GetPayment(ctx, "emv_tx_1002")
	if fetchedRefunded.Status != payments.PaymentRefunded {
		t.Errorf("expected status REFUNDED, got: %s", fetchedRefunded.Status)
	}
}

func TestSQLEMVPaymentRepository_NotFound(t *testing.T) {
	db, cleanup := setupTestDB(t, "test_payments_not_found")
	defer cleanup()

	repo := NewSQLEMVPaymentRepository(db)
	ctx := context.Background()

	_, found, err := repo.GetPayment(ctx, "non_existent_tx")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if found {
		t.Errorf("expected payment not found")
	}

	err = repo.TransitionStatus(ctx, "non_existent_tx", payments.PaymentAuthorized, payments.PaymentSettled)
	if !errors.Is(err, payments.ErrPaymentNotFound) {
		t.Errorf("expected ErrPaymentNotFound, got: %v", err)
	}
}
