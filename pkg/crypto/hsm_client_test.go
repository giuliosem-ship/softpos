package crypto

import (
	"bytes"
	"context"
	"crypto/des"
	"sync"
	"testing"
	"time"

	"softpos/pkg/iso8583"
)

func TestSoftwareHSMSimulator_GenerateZPK(t *testing.T) {
	hsm := NewSoftwareHSMSimulator()

	zpk1, err := hsm.GenerateZPK()
	if err != nil {
		t.Fatalf("GenerateZPK 1 failed: %v", err)
	}
	if len(zpk1) != 16 {
		t.Fatalf("Expected 16-byte ZPK, got %d", len(zpk1))
	}

	zpk2, err := hsm.GenerateZPK()
	if err != nil {
		t.Fatalf("GenerateZPK 2 failed: %v", err)
	}

	if bytes.Equal(zpk1, zpk2) {
		t.Fatal("Two generated ZPKs must be randomly distinct")
	}
}

func TestSoftwareHSMSimulator_TranslatePIN(t *testing.T) {
	hsm := NewSoftwareHSMSimulator()
	ctx := context.Background()

	bdk := decodeHexOrPanic("0123456789ABCDEFFEDCBA9876543210")
	ksn := decodeHexOrPanic("FFFF9876543210E00008")
	pan := "4111111111111111"
	pin := "7788"

	zpk, err := hsm.GenerateZPK()
	if err != nil {
		t.Fatalf("GenerateZPK failed: %v", err)
	}

	// Format plain ISO-0 PIN block and encrypt under terminal PEK
	plainBlock, err := FormatIso0PinBlock(pin, pan)
	if err != nil {
		t.Fatalf("FormatIso0PinBlock failed: %v", err)
	}

	incomingBlock, err := EncryptPinBlock(plainBlock, ksn, bdk)
	if err != nil {
		t.Fatalf("EncryptPinBlock failed: %v", err)
	}

	resp, err := hsm.TranslatePIN(ctx, PINTranslationRequest{
		IncomingPinBlock: incomingBlock,
		KSN:              ksn,
		BDK:              bdk,
		ZPK:              zpk,
		PAN:              pan,
	})
	if err != nil {
		t.Fatalf("TranslatePIN failed: %v", err)
	}
	if len(resp.TranslatedPinBlock) != 8 {
		t.Fatalf("Expected 8-byte translated PIN block, got %d", len(resp.TranslatedPinBlock))
	}

	// Verify decrypted under ZPK
	tripleZPK := append(zpk, zpk[:8]...)
	cipherZPK, err := des.NewTripleDESCipher(tripleZPK)
	if err != nil {
		t.Fatalf("Failed to initialize cipher: %v", err)
	}
	decrypted := make([]byte, 8)
	cipherZPK.Decrypt(decrypted, resp.TranslatedPinBlock)

	if !bytes.Equal(decrypted, plainBlock) {
		t.Fatalf("Decrypted translated PIN block does not match plaintext block.\nGot:  %X\nWant: %X", decrypted, plainBlock)
	}

	extractedPin, err := ExtractPinFromIso0(decrypted, pan)
	if err != nil {
		t.Fatalf("ExtractPinFromIso0 failed: %v", err)
	}
	if extractedPin != pin {
		t.Fatalf("Extracted PIN mismatch: got %s, want %s", extractedPin, pin)
	}
}

func TestSoftwareHSMSimulator_VerifyARQCAndGenerateARPC(t *testing.T) {
	hsm := NewSoftwareHSMSimulator()
	ctx := context.Background()

	sessionKey := decodeHexOrPanic("0123456789ABCDEFFEDCBA9876543210")
	txData := []byte("AMOUNT:150000;CURRENCY:484;STAN:123456")

	// 1. Generate valid ARQC
	arqc, err := iso8583.ComputeARQC(sessionKey, txData)
	if err != nil {
		t.Fatalf("ComputeARQC failed: %v", err)
	}

	// 2. Verify valid ARQC via HSM
	ok, err := hsm.VerifyARQC(ctx, ARQCVerifyRequest{
		SessionKey:      sessionKey,
		TransactionData: txData,
		CandidateARQC:   arqc,
	})
	if err != nil {
		t.Fatalf("VerifyARQC returned unexpected error: %v", err)
	}
	if !ok {
		t.Fatal("VerifyARQC expected true for valid ARQC, got false")
	}

	// 3. Verify corrupted ARQC returns false
	tamperedARQC := append([]byte(nil), arqc...)
	tamperedARQC[0] ^= 0xFF
	okTampered, err := hsm.VerifyARQC(ctx, ARQCVerifyRequest{
		SessionKey:      sessionKey,
		TransactionData: txData,
		CandidateARQC:   tamperedARQC,
	})
	if err != nil {
		t.Fatalf("VerifyARQC returned error on tampered candidate: %v", err)
	}
	if okTampered {
		t.Fatal("VerifyARQC expected false for tampered ARQC, got true")
	}

	// 4. Generate ARPC via HSM
	arpc, err := hsm.GenerateARPC(ctx, ARPCGenerateRequest{
		SessionKey:   sessionKey,
		ARQC:         arqc,
		ResponseCode: "00",
	})
	if err != nil {
		t.Fatalf("GenerateARPC failed: %v", err)
	}
	if len(arpc) != 8 {
		t.Fatalf("Expected 8-byte ARPC, got %d", len(arpc))
	}
}

func TestSoftwareHSMSimulator_ContextCancellation(t *testing.T) {
	hsm := NewSoftwareHSMSimulator()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel context

	bdk := decodeHexOrPanic("0123456789ABCDEFFEDCBA9876543210")
	ksn := decodeHexOrPanic("FFFF9876543210E00008")
	pan := "4111111111111111"
	zpk := decodeHexOrPanic("FEDCBA98765432100123456789ABCDEF")
	block := make([]byte, 8)

	_, err := hsm.TranslatePIN(ctx, PINTranslationRequest{
		IncomingPinBlock: block,
		KSN:              ksn,
		BDK:              bdk,
		ZPK:              zpk,
		PAN:              pan,
	})
	if err != context.Canceled {
		t.Fatalf("Expected context.Canceled, got: %v", err)
	}

	_, err = hsm.VerifyARQC(ctx, ARQCVerifyRequest{
		SessionKey:      bdk,
		TransactionData: []byte("TEST"),
		CandidateARQC:   block,
	})
	if err != context.Canceled {
		t.Fatalf("Expected context.Canceled for VerifyARQC, got: %v", err)
	}

	_, err = hsm.GenerateARPC(ctx, ARPCGenerateRequest{
		SessionKey:   bdk,
		ARQC:         block,
		ResponseCode: "00",
	})
	if err != context.Canceled {
		t.Fatalf("Expected context.Canceled for GenerateARPC, got: %v", err)
	}
}

func TestSoftwareHSMSimulator_ConcurrentSafety(t *testing.T) {
	hsm := NewSoftwareHSMSimulator()
	ctx := context.Background()

	bdk := decodeHexOrPanic("0123456789ABCDEFFEDCBA9876543210")
	ksn := decodeHexOrPanic("FFFF9876543210E00008")
	pan := "4111111111111111"
	pin := "1234"
	plainBlock, _ := FormatIso0PinBlock(pin, pan)
	incomingBlock, _ := EncryptPinBlock(plainBlock, ksn, bdk)
	sessionKey := decodeHexOrPanic("0123456789ABCDEFFEDCBA9876543210")
	txData := []byte("TEST CONCURRENCY")
	arqc, _ := iso8583.ComputeARQC(sessionKey, txData)

	const workers = 20
	const iterations = 10
	var wg sync.WaitGroup
	wg.Add(workers)

	for w := 0; w < workers; w++ {
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				// 1. Generate ZPK
				zpk, err := hsm.GenerateZPK()
				if err != nil || len(zpk) != 16 {
					t.Errorf("Concurrent GenerateZPK failed: %v", err)
					return
				}

				// 2. Translate PIN
				_, err = hsm.TranslatePIN(ctx, PINTranslationRequest{
					IncomingPinBlock: incomingBlock,
					KSN:              ksn,
					BDK:              bdk,
					ZPK:              zpk,
					PAN:              pan,
				})
				if err != nil {
					t.Errorf("Concurrent TranslatePIN failed: %v", err)
					return
				}

				// 3. Verify ARQC
				ok, err := hsm.VerifyARQC(ctx, ARQCVerifyRequest{
					SessionKey:      sessionKey,
					TransactionData: txData,
					CandidateARQC:   arqc,
				})
				if err != nil || !ok {
					t.Errorf("Concurrent VerifyARQC failed: %v", err)
					return
				}

				// 4. Generate ARPC
				_, err = hsm.GenerateARPC(ctx, ARPCGenerateRequest{
					SessionKey:   sessionKey,
					ARQC:         arqc,
					ResponseCode: "00",
				})
				if err != nil {
					t.Errorf("Concurrent GenerateARPC failed: %v", err)
					return
				}
			}
		}()
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// success
	case <-time.After(15 * time.Second):
		t.Fatal("Concurrent test timed out: possible deadlock")
	}
}
