package auth

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

var testSecret = []byte("test-secret-not-for-production")

func TestJWTRoundTrip(t *testing.T) {
	want := Claims{
		Subject: "u1", Email: "a@b.io", Role: RoleUser, Org: "acme", OrgRole: OrgOwner,
		IssuedAt: time.Now().Unix(), ExpiresAt: time.Now().Add(time.Minute).Unix(), TokenID: "s1",
	}
	token, err := signJWT(want, testSecret)
	if err != nil {
		t.Fatalf("signJWT: %v", err)
	}
	got, err := parseJWT(token, testSecret)
	if err != nil {
		t.Fatalf("parseJWT: %v", err)
	}
	if got != want {
		t.Errorf("claims = %+v, want %+v", got, want)
	}
}

func TestJWTRejectsTamperedPayload(t *testing.T) {
	token, err := signJWT(Claims{Subject: "u1", Role: RoleUser,
		ExpiresAt: time.Now().Add(time.Minute).Unix()}, testSecret)
	if err != nil {
		t.Fatalf("signJWT: %v", err)
	}
	parts := strings.Split(token, ".")

	// Promote the user to admin and re-encode, leaving the signature alone.
	raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims Claims
	_ = json.Unmarshal(raw, &claims)
	claims.Role = RoleAdmin
	forged, _ := json.Marshal(claims)
	parts[1] = base64.RawURLEncoding.EncodeToString(forged)

	if _, err := parseJWT(strings.Join(parts, "."), testSecret); err == nil {
		t.Fatal("a tampered payload verified")
	}
}

// The algorithm must come from the verifier, never from the token: "none" is
// the classic way to turn a signed token into an unsigned one.
func TestJWTRejectsAlgNone(t *testing.T) {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload, _ := json.Marshal(Claims{Subject: "u1", Role: RoleAdmin,
		ExpiresAt: time.Now().Add(time.Minute).Unix()})
	token := header + "." + base64.RawURLEncoding.EncodeToString(payload) + "."

	if _, err := parseJWT(token, testSecret); err == nil {
		t.Fatal("an alg=none token verified")
	}
}

func TestJWTRejectsWrongSecretAndExpiry(t *testing.T) {
	token, _ := signJWT(Claims{Subject: "u1", ExpiresAt: time.Now().Add(time.Minute).Unix()}, testSecret)
	if _, err := parseJWT(token, []byte("a-different-secret")); err != ErrBadToken {
		t.Errorf("wrong secret = %v, want ErrBadToken", err)
	}

	expired, _ := signJWT(Claims{Subject: "u1", ExpiresAt: time.Now().Add(-time.Second).Unix()}, testSecret)
	if _, err := parseJWT(expired, testSecret); err != ErrTokenExpired {
		t.Errorf("expired token = %v, want ErrTokenExpired", err)
	}
}

func TestJWTRejectsMalformed(t *testing.T) {
	for _, bad := range []string{"", "a.b", "a.b.c.d", "not-a-token", "...."} {
		if _, err := parseJWT(bad, testSecret); err == nil {
			t.Errorf("parseJWT(%q) accepted a malformed token", bad)
		}
	}
}

func TestRoleHierarchy(t *testing.T) {
	cases := []struct {
		have, want string
		ok         bool
	}{
		{OrgOwner, OrgMember, true},
		{OrgOwner, OrgAdmin, true},
		{OrgOwner, OrgOwner, true},
		{OrgAdmin, OrgOwner, false},
		{OrgAdmin, OrgAdmin, true},
		{OrgMember, OrgAdmin, false},
		{OrgMember, OrgMember, true},
		{"", OrgMember, false},
		{OrgOwner, "nonsense", false},
	}
	for _, tc := range cases {
		if got := AtLeast(tc.have, tc.want); got != tc.ok {
			t.Errorf("AtLeast(%q, %q) = %v, want %v", tc.have, tc.want, got, tc.ok)
		}
	}
}
