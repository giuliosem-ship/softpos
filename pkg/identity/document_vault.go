package identity

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
)

// ErrDocumentVault is returned when a KYC document cannot be sealed/opened.
var ErrDocumentVault = errors.New("KYC document cryptographic operation failed")

// DocumentEnvelope is the stored (encrypted) form of a KYC document: the
// content sealed with a per-document DEK (AES-256-GCM, the binding label as
// AAD) and the DEK wrapped by the vault KeyProvider under KeyID — the same
// envelope scheme as the CSD vault, so KMS rotation applies to both.
type DocumentEnvelope struct {
	KeyID      string `json:"key_id"`
	WrappedDEK []byte `json:"wrapped_dek"`
	Ciphertext []byte `json:"ciphertext"` // nonce || AES-GCM(content)
}

// DocumentVault seals KYC documents (INE, selfie, proofs) before they reach
// blob storage. The plaintext never leaves the process unencrypted.
type DocumentVault struct {
	keys KeyProvider
}

// NewDocumentVault builds a vault over the configured master-key provider.
func NewDocumentVault(p KeyProvider) (*DocumentVault, error) {
	if p == nil {
		return nil, ErrDocumentVault
	}
	return &DocumentVault{keys: p}, nil
}

// Seal encrypts content bound to label (e.g. "<merchant>/<document id>"):
// opening it under another label fails, so a blob cannot be swapped between
// documents. It returns the JSON-encoded DocumentEnvelope.
func (v *DocumentVault) Seal(ctx context.Context, label string, content []byte) ([]byte, error) {
	if label == "" || len(content) == 0 {
		return nil, ErrDocumentVault
	}
	dek := make([]byte, 32)
	if _, err := rand.Read(dek); err != nil {
		return nil, ErrDocumentVault
	}
	defer clear(dek)
	gcm, err := documentGCM(dek)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, ErrDocumentVault
	}
	ct := gcm.Seal(nonce, nonce, content, []byte(label))
	keyID, wrapped, err := v.keys.WrapDEK(ctx, dek)
	if err != nil {
		return nil, ErrDocumentVault
	}
	return json.Marshal(DocumentEnvelope{KeyID: keyID, WrappedDEK: wrapped, Ciphertext: ct})
}

// Open decrypts an envelope produced by Seal under the same label.
func (v *DocumentVault) Open(ctx context.Context, label string, envelope []byte) ([]byte, error) {
	var env DocumentEnvelope
	if err := json.Unmarshal(envelope, &env); err != nil || env.KeyID == "" {
		return nil, ErrDocumentVault
	}
	dek, err := v.keys.UnwrapDEK(ctx, env.KeyID, env.WrappedDEK)
	if err != nil {
		return nil, ErrDocumentVault
	}
	defer clear(dek)
	gcm, err := documentGCM(dek)
	if err != nil {
		return nil, err
	}
	if len(env.Ciphertext) <= gcm.NonceSize() {
		return nil, ErrDocumentVault
	}
	ns := gcm.NonceSize()
	out, err := gcm.Open(nil, env.Ciphertext[:ns], env.Ciphertext[ns:], []byte(label))
	if err != nil {
		return nil, ErrDocumentVault
	}
	return out, nil
}

func documentGCM(dek []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(dek)
	if err != nil {
		return nil, ErrDocumentVault
	}
	g, err := cipher.NewGCM(block)
	if err != nil {
		return nil, ErrDocumentVault
	}
	return g, nil
}
