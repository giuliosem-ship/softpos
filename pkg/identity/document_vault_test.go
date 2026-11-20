package identity

import (
	"bytes"
	"context"
	"testing"
)

func TestDocumentVault_SealOpenBoundToLabel(t *testing.T) {
	ctx := context.Background()
	v, err := NewDocumentVault(NewEphemeralKeyProvider())
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("%PDF-1.4 fake ine")
	env, err := v.Seal(ctx, "mer_1/doc_1", content)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(env, content) {
		t.Fatal("envelope contains the plaintext")
	}
	got, err := v.Open(ctx, "mer_1/doc_1", env)
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("open: %v %q", err, got)
	}
	if _, err := v.Open(ctx, "mer_2/doc_1", env); err == nil {
		t.Fatal("expected label mismatch to fail")
	}
	other, _ := NewDocumentVault(NewEphemeralKeyProvider())
	if _, err := other.Open(ctx, "mer_1/doc_1", env); err == nil {
		t.Fatal("expected another master key to fail")
	}
	if _, err := v.Seal(ctx, "", content); err == nil {
		t.Fatal("expected empty label to fail")
	}
	if _, err := NewDocumentVault(nil); err == nil {
		t.Fatal("expected nil provider to fail")
	}
}
