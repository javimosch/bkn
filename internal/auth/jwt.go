// Package auth is the identity primitive: users, organizations, memberships,
// and the tokens that carry them.
//
// It deliberately holds no billing state. In the Node backend it replaces,
// subscriptionStatus, currentPlan and stripeCustomerId lived on the user
// record, and the absence of an endpoint to write stripeCustomerId is what
// drove one consumer to bypass the API and write MongoDB directly. Billing is
// not identity; it belongs in a store collection, managed by a script.
package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	ErrBadToken     = errors.New("token is malformed or its signature does not match")
	ErrTokenExpired = errors.New("token has expired")
)

// Claims is the access-token payload. Field names follow the JWT registered
// claim names where one exists, so an off-the-shelf JWT library on the other
// side reads them without a custom mapper.
type Claims struct {
	Subject   string `json:"sub"`
	Email     string `json:"email"`
	Role      string `json:"role"`
	Org       string `json:"org,omitempty"`
	OrgRole   string `json:"org_role,omitempty"`
	IssuedAt  int64  `json:"iat"`
	ExpiresAt int64  `json:"exp"`
	TokenID   string `json:"jti"`
}

func b64(b []byte) string            { return base64.RawURLEncoding.EncodeToString(b) }
func unb64(s string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(s) }

// signJWT produces a compact HS256 JWT.
//
// Hand-rolled rather than pulled in as a dependency: this is one HMAC and two
// base64 segments, and the algorithm is pinned to HS256 at both ends, which
// also sidesteps the alg-confusion class of bug that comes free with a
// general-purpose verifier.
func signJWT(claims Claims, secret []byte) (string, error) {
	header := b64([]byte(`{"alg":"HS256","typ":"JWT"}`))
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	signing := header + "." + b64(payload)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(signing))
	return signing + "." + b64(mac.Sum(nil)), nil
}

// parseJWT verifies the signature and expiry, and returns the claims.
func parseJWT(token string, secret []byte) (Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Claims{}, ErrBadToken
	}
	var header struct {
		Alg string `json:"alg"`
		Typ string `json:"typ"`
	}
	rawHeader, err := unb64(parts[0])
	if err != nil || json.Unmarshal(rawHeader, &header) != nil {
		return Claims{}, ErrBadToken
	}
	// The algorithm is never taken from the token. "none" and an RSA public
	// key passed off as an HMAC secret are both refused by construction.
	if header.Alg != "HS256" {
		return Claims{}, fmt.Errorf("%w: unsupported alg %q", ErrBadToken, header.Alg)
	}

	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(parts[0] + "." + parts[1]))
	want := mac.Sum(nil)
	got, err := unb64(parts[2])
	if err != nil || !hmac.Equal(want, got) {
		return Claims{}, ErrBadToken
	}

	rawPayload, err := unb64(parts[1])
	if err != nil {
		return Claims{}, ErrBadToken
	}
	var claims Claims
	if err := json.Unmarshal(rawPayload, &claims); err != nil {
		return Claims{}, ErrBadToken
	}
	if claims.ExpiresAt > 0 && time.Now().Unix() >= claims.ExpiresAt {
		return Claims{}, ErrTokenExpired
	}
	return claims, nil
}
