package kv_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/javimosch/bkn/internal/db"
	"github.com/javimosch/bkn/internal/kv"
)

const testKeyHex = "6161616161616161616161616161616161616161616161616161616161616161"

func newKV(t *testing.T, keyEnv string) *kv.KV {
	t.Helper()
	t.Setenv("BKN_DATA", t.TempDir()+"/test.db")
	t.Setenv("BKN_ENCRYPTION_KEYS", "")
	t.Setenv("BKN_ENCRYPTION_KEY_ID", "")
	t.Setenv("BKN_ENCRYPTION_KEY", keyEnv)
	t.Setenv("SUPERBACKEND_ENCRYPTION_KEY", "")
	t.Setenv("SAASBACKEND_ENCRYPTION_KEY", "")

	conn, err := db.Open()
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	kr, err := kv.LoadKeyring()
	if err != nil {
		kr = nil
	}
	return kv.New(conn, kr, 0)
}

// The payload shape is a contract shared with the Node implementation and with
// external processes that encrypt or decrypt independently. Changing a field
// name or the encoding silently orphans every stored secret.
func TestEncryptedPayloadKeepsTheWireFormat(t *testing.T) {
	k := newKV(t, testKeyHex)
	if _, err := k.Set("a.secret", "hunter2", kv.TypeEncrypted, "", false); err != nil {
		t.Fatalf("Set: %v", err)
	}
	meta, err := k.Meta("a.secret")
	if err != nil {
		t.Fatalf("Meta: %v", err)
	}
	if meta.Value != "" {
		t.Error("Meta exposed an encrypted value")
	}

	kr, err := kv.LoadKeyring()
	if err != nil {
		t.Fatalf("LoadKeyring: %v", err)
	}
	sealed, err := kr.Encrypt("hunter2")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	var p kv.Payload
	if err := json.Unmarshal([]byte(sealed), &p); err != nil {
		t.Fatalf("payload is not JSON: %v", err)
	}
	if p.Alg != "aes-256-gcm" {
		t.Errorf("alg = %q, want aes-256-gcm", p.Alg)
	}
	if p.KeyID == "" || p.IV == "" || p.Tag == "" || p.Ciphertext == "" {
		t.Errorf("payload has an empty field: %+v", p)
	}
	back, err := kr.Decrypt(sealed)
	if err != nil || back != "hunter2" {
		t.Errorf("round trip = %q, %v", back, err)
	}
}

func TestKeyMaterialEncodings(t *testing.T) {
	// hex-64, base64-32 and 32 literal chars all denote the same 32 bytes.
	for name, material := range map[string]string{
		"hex":    testKeyHex,
		"base64": "YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWE=",
		"utf8":   strings.Repeat("a", 32),
	} {
		t.Run(name, func(t *testing.T) {
			k := newKV(t, material)
			if _, err := k.Set("x", "v", kv.TypeEncrypted, "", false); err != nil {
				t.Fatalf("Set with %s key: %v", name, err)
			}
			e, err := k.Get("x")
			if err != nil || e.Value != "v" {
				t.Fatalf("Get = %q, %v", e.Value, err)
			}
		})
	}
}

// Without a key, an encrypted write must fail rather than silently storing a
// secret in plaintext.
func TestEncryptedWriteFailsWithoutAKey(t *testing.T) {
	k := newKV(t, "")
	if _, err := k.Set("a.secret", "hunter2", kv.TypeEncrypted, "", false); err == nil {
		t.Fatal("Set succeeded with no encryption key configured")
	}
	if _, err := k.Get("a.secret"); err != kv.ErrNotFound {
		t.Errorf("a failed encrypted Set left something behind: %v", err)
	}
}

func TestEncryptedEntryCannotBePublic(t *testing.T) {
	k := newKV(t, testKeyHex)
	if _, err := k.Set("a.secret", "v", kv.TypeEncrypted, "", true); err == nil {
		t.Fatal("an encrypted entry was allowed to be public")
	}
}

func TestJSONTypeIsValidatedOnWrite(t *testing.T) {
	k := newKV(t, testKeyHex)
	if _, err := k.Set("a.cfg", "{nope}", kv.TypeJSON, "", false); err != kv.ErrBadJSON {
		t.Errorf("Set invalid JSON = %v, want ErrBadJSON", err)
	}
	if _, err := k.Set("a.cfg", `{"ok":true}`, kv.TypeJSON, "", false); err != nil {
		t.Errorf("Set valid JSON: %v", err)
	}
}

func TestListNeverRevealsEncryptedValues(t *testing.T) {
	k := newKV(t, testKeyHex)
	mustSet(t, k, "a.secret", "hunter2", kv.TypeEncrypted, false)
	mustSet(t, k, "a.plain", "visible", kv.TypeString, true)

	entries, err := k.List("a.", false)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, e := range entries {
		if e.Type == kv.TypeEncrypted && e.Value != "" {
			t.Errorf("List exposed the value of %q", e.Key)
		}
		if strings.Contains(e.Value, "hunter2") {
			t.Errorf("List leaked a secret in %q", e.Key)
		}
	}
}

// A write must drop the cached copy, otherwise a reader in the same process
// serves a stale value for the rest of the TTL.
func TestWriteInvalidatesTheCache(t *testing.T) {
	k := newKV(t, testKeyHex)
	mustSet(t, k, "a.n", "1", kv.TypeString, false)
	if e, _ := k.Get("a.n"); e.Value != "1" {
		t.Fatalf("first read = %q", e.Value)
	}
	mustSet(t, k, "a.n", "2", kv.TypeString, false)
	if e, _ := k.Get("a.n"); e.Value != "2" {
		t.Errorf("read after write = %q, want 2 (stale cache)", e.Value)
	}
	if err := k.Delete("a.n"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := k.Get("a.n"); err != kv.ErrNotFound {
		t.Errorf("read after delete = %v, want ErrNotFound", err)
	}
}

func TestRekeyRotatesAndReportsOrphans(t *testing.T) {
	dir := t.TempDir() + "/test.db"
	t.Setenv("BKN_DATA", dir)

	const k1 = testKeyHex
	const k2 = "6262626262626262626262626262626262626262626262626262626262626262"
	const orphan = "6363636363636363636363636363636363636363636363636363636363636363"

	open := func(keys, active string) *kv.KV {
		t.Setenv("BKN_ENCRYPTION_KEY", "")
		t.Setenv("BKN_ENCRYPTION_KEYS", keys)
		t.Setenv("BKN_ENCRYPTION_KEY_ID", active)
		conn, err := db.Open()
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		t.Cleanup(func() { conn.Close() })
		kr, err := kv.LoadKeyring()
		if err != nil {
			t.Fatalf("LoadKeyring: %v", err)
		}
		return kv.New(conn, kr, 0)
	}

	a := open("v1:"+k1, "v1")
	mustSet(t, a, "a.one", "one", kv.TypeEncrypted, false)

	// An entry sealed by a key that will not be in the keyring afterwards.
	b := open("v9:"+orphan, "v9")
	mustSet(t, b, "a.orphan", "lost", kv.TypeEncrypted, false)

	c := open("v1:"+k1+",v2:"+k2, "v2")
	res, err := c.Rekey()
	if err != nil {
		t.Fatalf("Rekey: %v", err)
	}
	if res.Rekeyed != 1 {
		t.Errorf("rekeyed = %d, want 1", res.Rekeyed)
	}
	if len(res.Failed) != 1 || res.Failed["a.orphan"] == "" {
		t.Errorf("failed = %v, want a.orphan reported", res.Failed)
	}
	if e, err := c.Get("a.one"); err != nil || e.Value != "one" {
		t.Errorf("rotated value = %q, %v", e.Value, err)
	}

	// A second rotation is a no-op for entries already on the active key.
	res, err = c.Rekey()
	if err != nil {
		t.Fatalf("second Rekey: %v", err)
	}
	if res.Rekeyed != 0 || res.Skipped != 1 {
		t.Errorf("second rekey = %+v, want 0 rekeyed / 1 skipped", res)
	}

	// The old key can now be retired without losing the rotated value.
	d := open("v2:"+k2, "v2")
	if e, err := d.Get("a.one"); err != nil || e.Value != "one" {
		t.Errorf("after retiring v1: %q, %v", e.Value, err)
	}
}

func mustSet(t *testing.T, k *kv.KV, key, val, typ string, public bool) {
	t.Helper()
	if _, err := k.Set(key, val, typ, "", public); err != nil {
		t.Fatalf("Set %q: %v", key, err)
	}
}
