package bootstrap

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/zakeripouya/EngramContextProtocol/internal/store"
)

// Pack signing (milestone M11): a pack is signed with an Ed25519 key over its
// canonical JSON (the pack with the signature removed). Installing checks the
// signature against keys the user chose to trust. Packs are data, never code, but
// they shape what agents recall, so their origin must be checkable.

// Signature is attached to a signed pack.
type Signature struct {
	KeyID     string `json:"keyId"`     // first 16 hex characters of sha256(public key)
	PublicKey string `json:"publicKey"` // base64
	Sig       string `json:"sig"`       // base64 Ed25519 signature of the canonical pack
}

// TrustedKey is a publisher key the user trusts.
type TrustedKey struct {
	KeyID     string `json:"keyId"`
	PublicKey string `json:"publicKey"`
	Name      string `json:"name"`
}

const settingTrusted = "pack.trusted"

// KeyID derives a short id from a public key.
func KeyID(pub ed25519.PublicKey) string {
	h := sha256.Sum256(pub)
	return hex.EncodeToString(h[:8])
}

func canonical(p Pack) ([]byte, error) {
	p.Signature = nil
	return json.Marshal(p)
}

// GenerateKey creates a signing key at path (mode 0600) and returns its public key.
// It refuses to overwrite an existing key.
func GenerateKey(path string) (ed25519.PublicKey, error) {
	if _, err := os.Stat(path); err == nil {
		return nil, fmt.Errorf("%s already exists", path)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	return pub, os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(priv)+"\n"), 0o600)
}

// LoadKey reads a signing key written by GenerateKey.
func LoadKey(path string) (ed25519.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
	if err != nil || len(raw) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("%s is not an Engram signing key", path)
	}
	return ed25519.PrivateKey(raw), nil
}

// Sign attaches a signature to a pack.
func Sign(p Pack, key ed25519.PrivateKey) (Pack, error) {
	msg, err := canonical(p)
	if err != nil {
		return p, err
	}
	pub := key.Public().(ed25519.PublicKey)
	p.Signature = &Signature{KeyID: KeyID(pub), PublicKey: base64.StdEncoding.EncodeToString(pub),
		Sig: base64.StdEncoding.EncodeToString(ed25519.Sign(key, msg))}
	return p, nil
}

// Errors from Verify.
var (
	ErrUnsigned  = errors.New("the pack is not signed")
	ErrBadSig    = errors.New("the pack's signature does not match its content; it was changed after signing")
	ErrUntrusted = errors.New("the pack is signed by a key you have not trusted")
)

// Verify checks a pack's signature. It returns the signing key id, and
// ErrUnsigned, ErrBadSig or ErrUntrusted when the pack can't be trusted.
func Verify(st *store.Store, p Pack) (string, error) {
	if p.Signature == nil {
		return "", ErrUnsigned
	}
	pub, err := base64.StdEncoding.DecodeString(p.Signature.PublicKey)
	if err != nil || len(pub) != ed25519.PublicKeySize || KeyID(pub) != p.Signature.KeyID {
		return p.Signature.KeyID, ErrBadSig
	}
	sig, err := base64.StdEncoding.DecodeString(p.Signature.Sig)
	if err != nil {
		return p.Signature.KeyID, ErrBadSig
	}
	msg, err := canonical(p)
	if err != nil {
		return p.Signature.KeyID, err
	}
	if !ed25519.Verify(pub, msg, sig) {
		return p.Signature.KeyID, ErrBadSig
	}
	keys, err := TrustedKeys(st)
	if err != nil {
		return p.Signature.KeyID, err
	}
	for _, k := range keys {
		if k.KeyID == p.Signature.KeyID && k.PublicKey == p.Signature.PublicKey {
			return k.KeyID, nil
		}
	}
	return p.Signature.KeyID, ErrUntrusted
}

// TrustedKeys lists the publisher keys the user trusts.
func TrustedKeys(st *store.Store) ([]TrustedKey, error) {
	v, err := st.Setting(settingTrusted)
	if err != nil || v == "" {
		return []TrustedKey{}, err
	}
	var keys []TrustedKey
	return keys, json.Unmarshal([]byte(v), &keys)
}

// Trust adds a publisher key (base64 public key) under a name.
func Trust(st *store.Store, publicKey, name string) (TrustedKey, error) {
	pub, err := base64.StdEncoding.DecodeString(strings.TrimSpace(publicKey))
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return TrustedKey{}, fmt.Errorf("not an Ed25519 public key (base64, 32 bytes)")
	}
	k := TrustedKey{KeyID: KeyID(pub), PublicKey: base64.StdEncoding.EncodeToString(pub), Name: name}
	keys, err := TrustedKeys(st)
	if err != nil {
		return k, err
	}
	for _, x := range keys {
		if x.KeyID == k.KeyID {
			return x, nil
		}
	}
	b, _ := json.Marshal(append(keys, k))
	return k, st.SetSetting(settingTrusted, string(b))
}

// Untrust removes a key by id or name.
func Untrust(st *store.Store, idOrName string) (int, error) {
	keys, err := TrustedKeys(st)
	if err != nil {
		return 0, err
	}
	var kept []TrustedKey
	for _, k := range keys {
		if k.KeyID != idOrName && k.Name != idOrName {
			kept = append(kept, k)
		}
	}
	if kept == nil {
		kept = []TrustedKey{}
	}
	b, _ := json.Marshal(kept)
	return len(keys) - len(kept), st.SetSetting(settingTrusted, string(b))
}
