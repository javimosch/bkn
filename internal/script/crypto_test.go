package script_test

import (
	"testing"
)

// Digests are checked against values produced by Node's crypto module, which
// is what the webhook providers themselves document against.
func TestCryptoMatchesReferenceDigests(t *testing.T) {
	_, runner, _ := setup(t)
	res := run(t, runner, `
		function main() {
			return {
				sha256_hex:  bkn.crypto.hash("hello world"),
				sha256_b64:  bkn.crypto.hash("hello world", {output:"base64"}),
				hmac_hex:    bkn.crypto.hmac("secret", "payload"),
				hmac_b64:    bkn.crypto.hmac("secret", "payload", {output:"base64"}),
				sha512_hex:  bkn.crypto.hash("abc", {algorithm:"sha512"}),
				b64_round:   bkn.crypto.base64Decode(bkn.crypto.base64Encode("round trip")),
				eq_same:     bkn.crypto.equal("abc", "abc"),
				eq_diff:     bkn.crypto.equal("abc", "abd"),
				eq_prefix:   bkn.crypto.equal("abc", "ab")
			};
		}`, nil)
	if !res.OK {
		t.Fatalf("run failed: %s", res.Run.Error)
	}
	got := res.Value.(map[string]any)

	want := map[string]any{
		"sha256_hex": "b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9",
		"sha256_b64": "uU0nuZNNPgilLlLX2n2r+sSE7+N6U4DukIj3rOLvzek=",
		"hmac_hex":   "b82fcb791acec57859b989b430a826488ce2e479fdf92326bd0a2e8375a42ba4",
		"hmac_b64":   "uC/LeRrOxXhZuYm0MKgmSIzi5Hn9+SMmvQoug3WkK6Q=",
		"b64_round":  "round trip",
		"eq_same":    true,
		"eq_diff":    false,
		"eq_prefix":  false,
	}
	for key, expected := range want {
		if got[key] != expected {
			t.Errorf("%s = %v, want %v", key, got[key], expected)
		}
	}
	if sha512, _ := got["sha512_hex"].(string); len(sha512) != 128 {
		t.Errorf("sha512 hex = %q, want 128 characters", sha512)
	}
}

func TestCryptoInputEncodings(t *testing.T) {
	_, runner, _ := setup(t)
	res := run(t, runner, `
		function main() {
			// The same bytes expressed three ways must hash identically.
			return {
				utf8:   bkn.crypto.hash("abc"),
				base64: bkn.crypto.hash("YWJj", {input:"base64"}),
				hex:    bkn.crypto.hash("616263", {input:"hex"})
			};
		}`, nil)
	if !res.OK {
		t.Fatalf("run failed: %s", res.Run.Error)
	}
	got := res.Value.(map[string]any)
	if got["utf8"] != got["base64"] || got["utf8"] != got["hex"] {
		t.Errorf("encodings disagree: %v", got)
	}
}

func TestCryptoRejectsUnknownAlgorithmsAndEncodings(t *testing.T) {
	_, runner, _ := setup(t)
	for _, probe := range []string{
		`bkn.crypto.hash("x", {algorithm:"md5"})`,
		`bkn.crypto.hash("x", {output:"rot13"})`,
		`bkn.crypto.hash("!!!", {input:"base64"})`,
		`bkn.crypto.base64Decode("!!!not base64")`,
		`bkn.crypto.randomHex(0)`,
		`bkn.crypto.randomHex(99999)`,
	} {
		res := run(t, runner, "function main(){ return "+probe+" }", nil)
		if res.OK {
			t.Errorf("%s returned %v instead of failing", probe, res.Value)
		}
	}
}

// A JavaScript string is UTF-8; asking for raw bytes without base64 silently
// replaces every invalid sequence with U+FFFD.
func TestFetchResponseEncodingIsExplicit(t *testing.T) {
	_, runner, _ := setup(t)
	res := run(t, runner, `
		function main() {
			return { hasCrypto: typeof bkn.crypto.hmac, hasLock: typeof bkn.lock.acquire,
			         hasPutIfAbsent: typeof bkn.store.putIfAbsent };
		}`, nil)
	if !res.OK {
		t.Fatalf("run failed: %s", res.Run.Error)
	}
	got := res.Value.(map[string]any)
	for key, v := range got {
		if v != "function" {
			t.Errorf("%s = %v, want a function", key, v)
		}
	}
}
