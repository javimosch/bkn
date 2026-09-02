// Package kv is the settings primitive: typed, optionally encrypted,
// optionally public key/value entries with a cache the core owns.
package kv

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

var (
	ErrNotFound = errors.New("key not found")
	ErrBadType  = errors.New("type must be one of: string, json, encrypted")
	ErrBadJSON  = errors.New("value is not valid JSON")
)

// Value types. Requirement R5.
const (
	TypeString    = "string"
	TypeJSON      = "json"
	TypeEncrypted = "encrypted"
)

func ValidTypes() []string { return []string{TypeString, TypeJSON, TypeEncrypted} }

// Entry is one setting. Value is always plaintext at this boundary: encrypted
// entries are sealed on write and opened on read, so no caller ever handles a
// payload envelope.
type Entry struct {
	Key         string `json:"key"`
	Value       string `json:"value"`
	Type        string `json:"type"`
	Description string `json:"description"`
	Public      bool   `json:"public"`
	UpdatedAt   string `json:"updated_at"`
}

// KV owns the settings table and its cache.
//
// Requirement R7: the cache lives here, once. Every external consumer of the
// Node backend built its own TTL cache with its own expiry and no way to
// invalidate a peer's copy. A write through this type drops the entry
// immediately, so a reader in the same process never serves a stale value.
type KV struct {
	db  *sql.DB
	kr  *Keyring
	ttl time.Duration

	mu    sync.RWMutex
	cache map[string]cached
}

type cached struct {
	entry Entry
	at    time.Time
}

// New builds a KV. keyring may be nil; encrypted entries then fail loudly
// rather than silently storing plaintext.
func New(db *sql.DB, kr *Keyring, ttl time.Duration) *KV {
	if ttl <= 0 {
		ttl = 60 * time.Second
	}
	return &KV{db: db, kr: kr, ttl: ttl, cache: map[string]cached{}}
}

func (k *KV) keyring() (*Keyring, error) {
	if k.kr == nil {
		return nil, ErrNoKey
	}
	return k.kr, nil
}

func (k *KV) invalidate(key string) {
	k.mu.Lock()
	delete(k.cache, key)
	k.mu.Unlock()
}

// InvalidateAll drops the whole cache.
func (k *KV) InvalidateAll() {
	k.mu.Lock()
	k.cache = map[string]cached{}
	k.mu.Unlock()
}

// Get returns one entry with its value in plaintext.
func (k *KV) Get(key string) (Entry, error) {
	k.mu.RLock()
	c, ok := k.cache[key]
	k.mu.RUnlock()
	if ok && time.Since(c.at) < k.ttl {
		return c.entry, nil
	}

	var e Entry
	var public int
	err := k.db.QueryRow(
		`SELECT key, value, type, description, public, updated_at FROM kv WHERE key = ?`, key).
		Scan(&e.Key, &e.Value, &e.Type, &e.Description, &public, &e.UpdatedAt)
	if err == sql.ErrNoRows {
		return Entry{}, ErrNotFound
	}
	if err != nil {
		return Entry{}, err
	}
	e.Public = public == 1

	if e.Type == TypeEncrypted {
		kr, err := k.keyring()
		if err != nil {
			return Entry{}, fmt.Errorf("cannot read %q: %w", key, err)
		}
		plain, err := kr.Decrypt(e.Value)
		if err != nil {
			return Entry{}, fmt.Errorf("cannot read %q: %w", key, err)
		}
		e.Value = plain
	}

	k.mu.Lock()
	k.cache[key] = cached{entry: e, at: time.Now()}
	k.mu.Unlock()
	return e, nil
}

// Meta returns an entry without decrypting it: encrypted values come back
// empty. Callers that must decide whether a reader is allowed to see an entry
// check its Public flag here first - attempting the decrypt before the access
// check would leak the existence of private keys through error messages.
func (k *KV) Meta(key string) (Entry, error) {
	var e Entry
	var public int
	err := k.db.QueryRow(
		`SELECT key, value, type, description, public, updated_at FROM kv WHERE key = ?`, key).
		Scan(&e.Key, &e.Value, &e.Type, &e.Description, &public, &e.UpdatedAt)
	if err == sql.ErrNoRows {
		return Entry{}, ErrNotFound
	}
	if err != nil {
		return Entry{}, err
	}
	e.Public = public == 1
	if e.Type == TypeEncrypted {
		e.Value = ""
	}
	return e, nil
}

// Set writes an entry, sealing it first when typ is "encrypted".
func (k *KV) Set(key, value, typ, description string, public bool) (Entry, error) {
	stored := value
	switch typ {
	case TypeString:
	case TypeJSON:
		if !json.Valid([]byte(value)) {
			return Entry{}, ErrBadJSON
		}
	case TypeEncrypted:
		kr, err := k.keyring()
		if err != nil {
			return Entry{}, err
		}
		sealed, err := kr.Encrypt(value)
		if err != nil {
			return Entry{}, err
		}
		stored = sealed
		if public {
			// An encrypted value served to unauthenticated readers is a
			// contradiction, not a configuration.
			return Entry{}, errors.New("an encrypted entry cannot be public")
		}
	default:
		return Entry{}, fmt.Errorf("%w: got %q", ErrBadType, typ)
	}

	now := time.Now().UTC().Format(time.RFC3339)
	pub := 0
	if public {
		pub = 1
	}
	_, err := k.db.Exec(`
		INSERT INTO kv (key, value, type, description, public, updated_at) VALUES (?,?,?,?,?,?)
		ON CONFLICT(key) DO UPDATE SET
		  value=excluded.value, type=excluded.type,
		  description=excluded.description, public=excluded.public,
		  updated_at=excluded.updated_at`,
		key, stored, typ, description, pub, now)
	if err != nil {
		return Entry{}, err
	}
	k.invalidate(key)
	return Entry{Key: key, Value: value, Type: typ, Description: description, Public: public, UpdatedAt: now}, nil
}

// List returns entries, never revealing encrypted values: those come back with
// an empty Value and must be fetched one at a time through Get.
func (k *KV) List(prefix string, publicOnly bool) ([]Entry, error) {
	q := `SELECT key, value, type, description, public, updated_at FROM kv WHERE 1=1`
	var args []any
	if prefix != "" {
		q += ` AND key LIKE ?`
		args = append(args, strings.ReplaceAll(prefix, "%", `\%`)+"%")
	}
	if publicOnly {
		q += ` AND public = 1`
	}
	q += ` ORDER BY key`

	rows, err := k.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Entry{}
	for rows.Next() {
		var e Entry
		var public int
		if err := rows.Scan(&e.Key, &e.Value, &e.Type, &e.Description, &public, &e.UpdatedAt); err != nil {
			return nil, err
		}
		e.Public = public == 1
		if e.Type == TypeEncrypted {
			e.Value = ""
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Delete removes an entry.
func (k *KV) Delete(key string) error {
	res, err := k.db.Exec(`DELETE FROM kv WHERE key = ?`, key)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	k.invalidate(key)
	return nil
}

// RekeyResult reports what a rotation moved and what it could not.
type RekeyResult struct {
	Rekeyed  int               `json:"rekeyed"`
	Skipped  int               `json:"skipped"`
	Failed   map[string]string `json:"failed,omitempty"`
	ActiveID string            `json:"active_key_id"`
}

// Rekey re-encrypts every encrypted entry under the active key.
//
// Requirement R6: keyId was in the payload format from the start but nothing
// ever read it, so rotation was impossible in practice.
//
// An entry sealed by a key no longer in the keyring is reported, not fatal:
// one orphaned value must not block rotating everything else. The caller gets
// a non-empty Failed map and decides.
func (k *KV) Rekey() (RekeyResult, error) {
	kr, err := k.keyring()
	if err != nil {
		return RekeyResult{}, err
	}
	res := RekeyResult{ActiveID: kr.ActiveKeyID(), Failed: map[string]string{}}

	rows, err := k.db.Query(`SELECT key, value FROM kv WHERE type = ?`, TypeEncrypted)
	if err != nil {
		return res, err
	}
	type pair struct{ key, val string }
	var pending []pair
	for rows.Next() {
		var p pair
		if err := rows.Scan(&p.key, &p.val); err != nil {
			rows.Close()
			return res, err
		}
		pending = append(pending, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return res, err
	}

	for _, p := range pending {
		var cur Payload
		if err := json.Unmarshal([]byte(p.val), &cur); err == nil && cur.KeyID == kr.ActiveKeyID() {
			res.Skipped++
			continue
		}
		plain, err := kr.Decrypt(p.val)
		if err != nil {
			res.Failed[p.key] = err.Error()
			continue
		}
		sealed, err := kr.Encrypt(plain)
		if err != nil {
			res.Failed[p.key] = err.Error()
			continue
		}
		if _, err := k.db.Exec(`UPDATE kv SET value=?, updated_at=? WHERE key=?`,
			sealed, time.Now().UTC().Format(time.RFC3339), p.key); err != nil {
			return res, err
		}
		k.invalidate(p.key)
		res.Rekeyed++
	}
	return res, nil
}
