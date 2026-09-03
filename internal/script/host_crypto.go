package script

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"hash"
)

// newCryptoAPI builds the `bkn.crypto` namespace.
//
// It exists for one reason: verifying inbound webhook signatures. Every
// provider worth integrating with signs its payloads, and without an HMAC a
// script cannot check one - which would mean every signed integration had to
// be Go code, defeating the point of the sandbox. The primitives here are
// deliberately the small set that job needs.
func (r *Runner) newCryptoAPI(throw func(error)) map[string]any {
	decode := func(s, encoding string) []byte {
		switch encoding {
		case "", "utf8":
			return []byte(s)
		case "base64":
			raw, err := base64.StdEncoding.DecodeString(s)
			if err != nil {
				throw(fmt.Errorf("input is not valid base64: %w", err))
			}
			return raw
		case "hex":
			raw, err := hex.DecodeString(s)
			if err != nil {
				throw(fmt.Errorf("input is not valid hex: %w", err))
			}
			return raw
		default:
			throw(fmt.Errorf("unknown encoding %q; use utf8, base64 or hex", encoding))
			return nil
		}
	}
	encode := func(b []byte, encoding string) string {
		switch encoding {
		case "", "hex":
			return hex.EncodeToString(b)
		case "base64":
			return base64.StdEncoding.EncodeToString(b)
		default:
			throw(fmt.Errorf("unknown output encoding %q; use hex or base64", encoding))
			return ""
		}
	}
	hasher := func(algorithm string) func() hash.Hash {
		switch algorithm {
		case "", "sha256":
			return sha256.New
		case "sha512":
			return sha512.New
		case "sha1":
			// Some older providers still sign with SHA-1. Available for
			// compatibility, never a default.
			return sha1.New
		default:
			throw(fmt.Errorf("unknown algorithm %q; use sha256, sha512 or sha1", algorithm))
			return nil
		}
	}
	opt := func(opts map[string]any, key string) string {
		if opts == nil {
			return ""
		}
		if v, ok := opts[key].(string); ok {
			return v
		}
		return ""
	}

	return map[string]any{
		"hash": func(data string, opts map[string]any) string {
			h := hasher(opt(opts, "algorithm"))()
			h.Write(decode(data, opt(opts, "input")))
			return encode(h.Sum(nil), opt(opts, "output"))
		},
		"hmac": func(key, data string, opts map[string]any) string {
			m := hmac.New(hasher(opt(opts, "algorithm")), decode(key, opt(opts, "keyInput")))
			m.Write(decode(data, opt(opts, "input")))
			return encode(m.Sum(nil), opt(opts, "output"))
		},
		// equal compares in constant time. A signature check written as
		// a === b leaks the correct prefix through timing; providers publish
		// this warning for a reason.
		"equal": func(a, b string) bool {
			return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
		},
		"base64Encode": func(s string) string {
			return base64.StdEncoding.EncodeToString([]byte(s))
		},
		"base64Decode": func(s string) string {
			raw, err := base64.StdEncoding.DecodeString(s)
			if err != nil {
				throw(fmt.Errorf("input is not valid base64: %w", err))
			}
			return string(raw)
		},
		"randomHex": func(n int) string {
			if n <= 0 || n > 1024 {
				throw(fmt.Errorf("randomHex needs 1-1024 bytes, got %d", n))
			}
			buf := make([]byte, n)
			if _, err := rand.Read(buf); err != nil {
				throw(err)
			}
			return hex.EncodeToString(buf)
		},
	}
}
