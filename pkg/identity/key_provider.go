package identity

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
)

var (
	// ErrInvalidMasterKey: a master key must be exactly 32 bytes (64 hex chars).
	ErrInvalidMasterKey = errors.New("vault master key must be 32 bytes hex-encoded")
	// ErrUnknownKeyID: the envelope was wrapped with a key this provider does not hold.
	ErrUnknownKeyID = errors.New("vault key id not available")
)

// KeyProvider wraps and unwraps data-encryption keys (DEKs) with a master key
// it controls. The key id is stored in each envelope, so keys can be rotated:
// new envelopes use the active key and old ones keep decrypting while their key
// is still provided (see CSDVaultService.RewrapCSD to migrate them).
//
// Implementations: StaticKeyProvider (keys from env, VAULT_MASTER_KEY_HEX +
// VAULT_PREVIOUS_KEYS) and KMSKeyProvider (a cloud KMS behind KMSClient).
type KeyProvider interface {
	// WrapDEK encrypts dek with the active master key.
	WrapDEK(ctx context.Context, dek []byte) (keyID string, wrapped []byte, err error)
	// UnwrapDEK decrypts a DEK wrapped under keyID. keyID "" marks a legacy
	// envelope (pre key-id format); providers may refuse it.
	UnwrapDEK(ctx context.Context, keyID string, wrapped []byte) ([]byte, error)
}

// StaticKeyProvider holds AES-256 master keys in process memory (loaded from
// the environment). wrapped = nonce(12) || AES-GCM(dek, aad = key id).
type StaticKeyProvider struct {
	activeID string
	keys     map[string][]byte
}

var _ KeyProvider = (*StaticKeyProvider)(nil)

// DecodeMasterKeyHex validates and decodes a 32-byte hex master key.
func DecodeMasterKeyHex(keyHex string) ([]byte, error) {
	k, err := hex.DecodeString(strings.TrimSpace(keyHex))
	if err != nil || len(k) != 32 {
		return nil, ErrInvalidMasterKey
	}
	return k, nil
}

// IsTrivialMasterKey reports whether k is a degenerate key (every byte equal,
// e.g. all zeros): a placeholder, never a real random key.
func IsTrivialMasterKey(k []byte) bool {
	for _, b := range k {
		if b != k[0] {
			return false
		}
	}
	return true
}

// NewStaticKeyProvider builds a provider whose active key is activeHex under
// activeID; previous maps retired key ids to hex keys (still able to unwrap).
func NewStaticKeyProvider(activeID, activeHex string, previous map[string]string) (*StaticKeyProvider, error) {
	if activeID == "" {
		return nil, errors.New("vault key id must not be empty")
	}
	active, err := DecodeMasterKeyHex(activeHex)
	if err != nil {
		return nil, err
	}
	p := &StaticKeyProvider{activeID: activeID, keys: map[string][]byte{activeID: active}}
	for id, h := range previous {
		if id == "" || id == activeID {
			return nil, fmt.Errorf("invalid previous vault key id %q", id)
		}
		k, err := DecodeMasterKeyHex(h)
		if err != nil {
			return nil, fmt.Errorf("previous vault key %q: %w", id, err)
		}
		p.keys[id] = k
	}
	return p, nil
}

// NewEphemeralKeyProvider returns a provider with a random per-process key
// (tests / unconfigured in-memory servers: envelopes die with the process).
func NewEphemeralKeyProvider() *StaticKeyProvider {
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	return &StaticKeyProvider{activeID: "ephemeral", keys: map[string][]byte{"ephemeral": k}}
}

// ParseKeyring parses VAULT_PREVIOUS_KEYS: "id1:hex1,id2:hex2" (empty -> none).
func ParseKeyring(spec string) (map[string]string, error) {
	out := map[string]string{}
	for i, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		id, h, ok := strings.Cut(part, ":")
		if !ok || strings.TrimSpace(id) == "" {
			// Never echo the entry: it is (or contains) key material.
			return nil, fmt.Errorf("invalid keyring entry #%d (want id:hex)", i+1)
		}
		out[strings.TrimSpace(id)] = strings.TrimSpace(h)
	}
	return out, nil
}

// ActiveKeyID returns the id used to wrap new DEKs.
func (p *StaticKeyProvider) ActiveKeyID() string { return p.activeID }

func gcmFor(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func (p *StaticKeyProvider) WrapDEK(_ context.Context, dek []byte) (string, []byte, error) {
	g, err := gcmFor(p.keys[p.activeID])
	if err != nil {
		return "", nil, ErrCSDVault
	}
	nonce := make([]byte, g.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", nil, ErrCSDVault
	}
	return p.activeID, g.Seal(nonce, nonce, dek, []byte(p.activeID)), nil
}

func (p *StaticKeyProvider) UnwrapDEK(_ context.Context, keyID string, wrapped []byte) ([]byte, error) {
	if keyID == "" {
		return p.unwrapLegacy(wrapped)
	}
	key, ok := p.keys[keyID]
	if !ok {
		return nil, ErrUnknownKeyID
	}
	g, err := gcmFor(key)
	if err != nil || len(wrapped) <= g.NonceSize() {
		return nil, ErrCSDVault
	}
	dek, err := g.Open(nil, wrapped[:g.NonceSize()], wrapped[g.NonceSize():], []byte(keyID))
	if err != nil {
		return nil, ErrCSDVault
	}
	return dek, nil
}

// unwrapLegacy opens pre key-id envelopes (IV || AES-GCM(dek), no AAD) with
// any held key, active first.
func (p *StaticKeyProvider) unwrapLegacy(wrapped []byte) ([]byte, error) {
	ids := make([]string, 0, len(p.keys))
	for id := range p.keys {
		if id != p.activeID {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range append([]string{p.activeID}, ids...) {
		g, err := gcmFor(p.keys[id])
		if err != nil || len(wrapped) <= g.NonceSize() {
			continue
		}
		if dek, err := g.Open(nil, wrapped[:g.NonceSize()], wrapped[g.NonceSize():], nil); err == nil {
			return dek, nil
		}
	}
	return nil, ErrCSDVault
}

// KMSClient is the minimal surface of a cloud KMS (AWS KMS Encrypt/Decrypt,
// GCP Cloud KMS, Vault Transit). Kept as an interface so no SDK is linked
// until a provider is chosen; the master key never leaves the KMS.
type KMSClient interface {
	Encrypt(ctx context.Context, keyID string, plaintext []byte) ([]byte, error)
	Decrypt(ctx context.Context, keyID string, ciphertext []byte) ([]byte, error)
}

// KMSKeyProvider wraps DEKs with a KMS key. KeyID is the KMS key ARN/name and
// is stored in each envelope; rotation is handled by the KMS (key versions)
// or by switching KeyID and re-wrapping. Legacy (pre key-id) envelopes are not
// supported: re-wrap them with the static provider before switching.
type KMSKeyProvider struct {
	Client KMSClient
	KeyID  string
}

var _ KeyProvider = (*KMSKeyProvider)(nil)

func (p *KMSKeyProvider) WrapDEK(ctx context.Context, dek []byte) (string, []byte, error) {
	if p.Client == nil || p.KeyID == "" {
		return "", nil, errors.New("KMS key provider not configured")
	}
	ct, err := p.Client.Encrypt(ctx, p.KeyID, dek)
	if err != nil {
		return "", nil, fmt.Errorf("kms encrypt: %w", err)
	}
	return p.KeyID, ct, nil
}

func (p *KMSKeyProvider) UnwrapDEK(ctx context.Context, keyID string, wrapped []byte) ([]byte, error) {
	if p.Client == nil {
		return nil, errors.New("KMS key provider not configured")
	}
	if keyID == "" {
		return nil, ErrUnknownKeyID
	}
	dek, err := p.Client.Decrypt(ctx, keyID, wrapped)
	if err != nil {
		return nil, fmt.Errorf("kms decrypt: %w", err)
	}
	return dek, nil
}
