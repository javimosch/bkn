package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/javimosch/bkn/internal/store"
)

// Session is a refresh-token record.
type Session struct {
	ID        string `json:"id"`
	UserID    string `json:"user_id"`
	Org       string `json:"org,omitempty"`
	CreatedAt string `json:"created_at"`
	ExpiresAt string `json:"expires_at"`
	RevokedAt string `json:"revoked_at,omitempty"`
}

// hashToken stores refresh tokens as digests.
//
// A refresh token is a bearer credential with a 30-day life. Storing the raw
// value would mean a read of the database is a read of every live session, so
// only the digest is kept and the plaintext is returned exactly once.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func newRefreshToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// Login verifies a password and issues a token pair.
func (a *Auth) Login(email, password, orgIDOrSlug string) (Tokens, error) {
	u, err := a.FindUser(email)
	if errors.Is(err, ErrUserNotFound) {
		// Verify against a dummy hash anyway so a missing account and a wrong
		// password take the same time. Without it, response latency tells an
		// attacker which addresses are registered.
		_ = bcrypt.CompareHashAndPassword(
			[]byte("$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy"), []byte(password))
		return Tokens{}, ErrBadCredentials
	}
	if err != nil {
		return Tokens{}, err
	}
	if bcrypt.CompareHashAndPassword([]byte(u.passwordHash), []byte(password)) != nil {
		return Tokens{}, ErrBadCredentials
	}
	if u.Disabled {
		return Tokens{}, ErrUserDisabled
	}
	return a.issue(u, orgIDOrSlug)
}

// issue mints an access/refresh pair, optionally scoped to an organization.
func (a *Auth) issue(u User, orgIDOrSlug string) (Tokens, error) {
	var orgSlug, orgRole string
	if orgIDOrSlug != "" {
		o, err := a.FindOrg(orgIDOrSlug)
		if err != nil {
			return Tokens{}, err
		}
		role, err := a.membershipRole(o.ID, u.ID)
		if err != nil {
			return Tokens{}, err
		}
		orgSlug, orgRole = o.Slug, role
	}

	sessionID := store.NewID()
	issuedAt := time.Now()
	claims := Claims{
		Subject:   u.ID,
		Email:     u.Email,
		Role:      u.Role,
		Org:       orgSlug,
		OrgRole:   orgRole,
		IssuedAt:  issuedAt.Unix(),
		ExpiresAt: issuedAt.Add(AccessTTL).Unix(),
		TokenID:   sessionID,
	}
	secret, err := a.signingKey()
	if err != nil {
		return Tokens{}, err
	}
	access, err := signJWT(claims, secret)
	if err != nil {
		return Tokens{}, err
	}
	refresh, err := newRefreshToken()
	if err != nil {
		return Tokens{}, err
	}
	if _, err := a.db.Exec(`
		INSERT INTO sessions (id, user_id, token_hash, org_id, expires_at, created_at)
		VALUES (?,?,?,?,?,?)`,
		sessionID, u.ID, hashToken(refresh), orgSlug,
		issuedAt.Add(RefreshTTL).UTC().Format(time.RFC3339), now()); err != nil {
		return Tokens{}, err
	}

	return Tokens{
		AccessToken:  access,
		RefreshToken: refresh,
		TokenType:    "Bearer",
		ExpiresIn:    int(AccessTTL.Seconds()),
		User:         u,
		Org:          orgSlug,
		OrgRole:      orgRole,
	}, nil
}

// IssueFor mints a token pair for a user without a password, which is what an
// SSO callback or an invite acceptance needs. It is the one operation here
// that bypasses credentials, so it is only reachable from the CLI-equivalent
// surfaces the operator controls - never from an unauthenticated route.
func (a *Auth) IssueFor(idOrEmail, orgIDOrSlug string) (Tokens, error) {
	u, err := a.FindUser(idOrEmail)
	if err != nil {
		return Tokens{}, err
	}
	if u.Disabled {
		return Tokens{}, ErrUserDisabled
	}
	return a.issue(u, orgIDOrSlug)
}

// Verify validates an access token and returns its claims. It is stateless: a
// revoked session's access token stays valid until it expires, which is why
// AccessTTL is short.
func (a *Auth) Verify(token string) (Claims, error) {
	secret, err := a.signingKey()
	if err != nil {
		return Claims{}, err
	}
	return parseJWT(token, secret)
}

// Me resolves an access token to the live user record, so a disabled or
// deleted account stops working before its access token expires.
func (a *Auth) Me(token string) (User, Claims, error) {
	claims, err := a.Verify(token)
	if err != nil {
		return User{}, Claims{}, err
	}
	u, err := a.FindUser(claims.Subject)
	if err != nil {
		return User{}, Claims{}, err
	}
	if u.Disabled {
		return User{}, Claims{}, ErrUserDisabled
	}
	return u, claims, nil
}

// lookupSession finds a live session by refresh token.
func (a *Auth) lookupSession(refresh string) (string, string, string, error) {
	var id, userID, orgSlug, expiresAt, revokedAt, storedHash string
	err := a.db.QueryRow(`
		SELECT id, user_id, org_id, expires_at, revoked_at, token_hash
		FROM sessions WHERE token_hash = ?`, hashToken(refresh)).
		Scan(&id, &userID, &orgSlug, &expiresAt, &revokedAt, &storedHash)
	if err == sql.ErrNoRows {
		return "", "", "", ErrSessionInvalid
	}
	if err != nil {
		return "", "", "", err
	}
	if subtle.ConstantTimeCompare([]byte(storedHash), []byte(hashToken(refresh))) != 1 {
		return "", "", "", ErrSessionInvalid
	}
	if revokedAt != "" {
		return "", "", "", ErrSessionInvalid
	}
	exp, err := time.Parse(time.RFC3339, expiresAt)
	if err != nil || time.Now().After(exp) {
		return "", "", "", ErrSessionInvalid
	}
	return id, userID, orgSlug, nil
}

// Refresh exchanges a refresh token for a new pair and retires the old one.
//
// Rotation is unconditional: a refresh token is single-use, so a stolen one
// stops working the moment the legitimate holder refreshes.
func (a *Auth) Refresh(refresh string) (Tokens, error) {
	sessionID, userID, orgSlug, err := a.lookupSession(refresh)
	if err != nil {
		return Tokens{}, err
	}
	u, err := a.FindUser(userID)
	if err != nil {
		return Tokens{}, err
	}
	if u.Disabled {
		return Tokens{}, ErrUserDisabled
	}
	if _, err := a.db.Exec(`UPDATE sessions SET revoked_at = ? WHERE id = ?`, now(), sessionID); err != nil {
		return Tokens{}, err
	}
	return a.issue(u, orgSlug)
}

// SwitchOrg exchanges a refresh token for one scoped to another organization,
// which requires the user to actually be a member of it.
func (a *Auth) SwitchOrg(refresh, orgIDOrSlug string) (Tokens, error) {
	sessionID, userID, _, err := a.lookupSession(refresh)
	if err != nil {
		return Tokens{}, err
	}
	u, err := a.FindUser(userID)
	if err != nil {
		return Tokens{}, err
	}
	if u.Disabled {
		return Tokens{}, ErrUserDisabled
	}
	tokens, err := a.issue(u, orgIDOrSlug)
	if err != nil {
		return Tokens{}, err
	}
	if _, err := a.db.Exec(`UPDATE sessions SET revoked_at = ? WHERE id = ?`, now(), sessionID); err != nil {
		return Tokens{}, err
	}
	return tokens, nil
}

// Logout revokes one session. Revoking an unknown or already-revoked token is
// a success: the caller's goal - that this token stops working - is met.
func (a *Auth) Logout(refresh string) error {
	_, err := a.db.Exec(`UPDATE sessions SET revoked_at = ? WHERE token_hash = ? AND revoked_at = ''`,
		now(), hashToken(refresh))
	return err
}

// RevokeAllSessions ends every session for a user and reports how many.
func (a *Auth) RevokeAllSessions(userIDOrEmail string) (int, error) {
	u, err := a.FindUser(userIDOrEmail)
	if err != nil {
		return 0, err
	}
	res, err := a.db.Exec(`UPDATE sessions SET revoked_at = ? WHERE user_id = ? AND revoked_at = ''`,
		now(), u.ID)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// Sessions lists a user's sessions, newest first.
func (a *Auth) Sessions(userIDOrEmail string) ([]Session, error) {
	u, err := a.FindUser(userIDOrEmail)
	if err != nil {
		return nil, err
	}
	rows, err := a.db.Query(`
		SELECT id, user_id, org_id, created_at, expires_at, revoked_at
		FROM sessions WHERE user_id = ? ORDER BY created_at DESC`, u.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Session{}
	for rows.Next() {
		var s Session
		if err := rows.Scan(&s.ID, &s.UserID, &s.Org, &s.CreatedAt, &s.ExpiresAt, &s.RevokedAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
