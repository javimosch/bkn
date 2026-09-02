package kv

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

// Payload is the on-disk shape of an encrypted value.
//
// Requirement R6: this format is a real contract, not an implementation
// detail - the Node backend, polybot and other external processes all
// encrypt and decrypt it independently. Field names, base64 encoding and the
// AES-256-GCM algorithm are preserved exactly. Only key rotation is new:
// keyId existed in the original payload but nothing ever read it.
type Payload struct {
	Alg        string `json:"alg"`
	KeyID      string `json:"keyId"`
	IV         string `json:"iv"`
	Tag        string `json:"tag"`
	Ciphertext string `json:"ciphertext"`
}

const algAESGCM = "aes-256-gcm"

var (
	ErrNoKey       = errors.New("no encryption key configured")
	ErrBadKey      = errors.New("encryption key must be 32 bytes (hex-64, base64-32, or utf8-32)")
	ErrUnknownKey  = errors.New("no key configured for that keyId")
	ErrBadPayload  = errors.New("malformed encrypted payload")
	ErrUnsupported = errors.New("unsupported encryption algorithm")
)

// Keyring holds every key that can decrypt, and the one id that encrypts.
type Keyring struct {
	keys   map[string][]byte
	active string
}

// parseKeyMaterial accepts the three encodings the original implementation
// accepted, in the same precedence order.
func parseKeyMaterial(raw string) ([]byte, error) {
	if len(raw) == 64 {
		if b, err := hex.DecodeString(raw); err == nil {
			return b, nil
		}
	}
	if b, err := base64.StdEncoding.DecodeString(raw); err == nil && len(b) == 32 {
		return b, nil
	}
	if len(raw) == 32 {
		return []byte(raw), nil
	}
	return nil, ErrBadKey
}

// LoadKeyring reads the keyring from the environment.
//
//	BKN_ENCRYPTION_KEYS="v1:<material>,v2:<material>"   multi-key, enables rotation
//	BKN_ENCRYPTION_KEY_ID="v2"                          which one encrypts
//	BKN_ENCRYPTION_KEY="<material>"                     single key, id "v1"
//
// SUPERBACKEND_ENCRYPTION_KEY and SAASBACKEND_ENCRYPTION_KEY are read as
// fallbacks so data written by the Node backend decrypts unchanged.
func LoadKeyring() (*Keyring, error) {
	kr := &Keyring{keys: map[string][]byte{}}

	if multi := os.Getenv("BKN_ENCRYPTION_KEYS"); multi != "" {
		for _, pair := range strings.Split(multi, ",") {
			id, material, ok := strings.Cut(strings.TrimSpace(pair), ":")
			if !ok {
				return nil, fmt.Errorf("BKN_ENCRYPTION_KEYS entry must be <keyId>:<material>, got %q", pair)
			}
			b, err := parseKeyMaterial(material)
			if err != nil {
				return nil, fmt.Errorf("key %q: %w", id, err)
			}
			kr.keys[id] = b
			kr.active = id // last wins unless BKN_ENCRYPTION_KEY_ID says otherwise
		}
	}

	for _, env := range []string{"BKN_ENCRYPTION_KEY", "SUPERBACKEND_ENCRYPTION_KEY", "SAASBACKEND_ENCRYPTION_KEY"} {
		raw := os.Getenv(env)
		if raw == "" {
			continue
		}
		b, err := parseKeyMaterial(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", env, err)
		}
		if _, exists := kr.keys["v1"]; !exists {
			kr.keys["v1"] = b
			if kr.active == "" {
				kr.active = "v1"
			}
		}
		break
	}

	if id := os.Getenv("BKN_ENCRYPTION_KEY_ID"); id != "" {
		if _, ok := kr.keys[id]; !ok {
			return nil, fmt.Errorf("%w: BKN_ENCRYPTION_KEY_ID=%s", ErrUnknownKey, id)
		}
		kr.active = id
	}
	if len(kr.keys) == 0 {
		return nil, ErrNoKey
	}
	return kr, nil
}

// ActiveKeyID reports which key new values are encrypted with.
func (k *Keyring) ActiveKeyID() string { return k.active }

// KeyIDs reports every key id that can decrypt.
func (k *Keyring) KeyIDs() []string {
	ids := make([]string, 0, len(k.keys))
	for id := range k.keys {
		ids = append(ids, id)
	}
	return ids
}

// Encrypt seals plaintext with the active key.
func (k *Keyring) Encrypt(plaintext string) (string, error) {
	key, ok := k.keys[k.active]
	if !ok {
		return "", ErrNoKey
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	iv := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(iv); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nil, iv, []byte(plaintext), nil)
	// Go appends the tag to the ciphertext; the wire format keeps them apart.
	split := len(sealed) - gcm.Overhead()
	p := Payload{
		Alg:        algAESGCM,
		KeyID:      k.active,
		IV:         base64.StdEncoding.EncodeToString(iv),
		Tag:        base64.StdEncoding.EncodeToString(sealed[split:]),
		Ciphertext: base64.StdEncoding.EncodeToString(sealed[:split]),
	}
	b, err := json.Marshal(p)
	return string(b), err
}

// Decrypt opens a payload using the key its keyId names.
func (k *Keyring) Decrypt(raw string) (string, error) {
	var p Payload
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return "", ErrBadPayload
	}
	if p.Alg != algAESGCM {
		return "", fmt.Errorf("%w: %s", ErrUnsupported, p.Alg)
	}
	id := p.KeyID
	if id == "" {
		id = "v1"
	}
	key, ok := k.keys[id]
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrUnknownKey, id)
	}
	iv, err1 := base64.StdEncoding.DecodeString(p.IV)
	tag, err2 := base64.StdEncoding.DecodeString(p.Tag)
	ct, err3 := base64.StdEncoding.DecodeString(p.Ciphertext)
	if err1 != nil || err2 != nil || err3 != nil {
		return "", ErrBadPayload
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	plain, err := gcm.Open(nil, iv, append(ct, tag...), nil)
	if err != nil {
		return "", fmt.Errorf("decrypt failed (wrong key or tampered value): %w", err)
	}
	return string(plain), nil
}
