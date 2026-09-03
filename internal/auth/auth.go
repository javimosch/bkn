package auth

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/javimosch/bkn/internal/kv"
	"github.com/javimosch/bkn/internal/store"
)

var (
	ErrUserNotFound   = errors.New("user not found")
	ErrOrgNotFound    = errors.New("organization not found")
	ErrEmailTaken     = errors.New("a user with that email already exists")
	ErrSlugTaken      = errors.New("an organization with that slug already exists")
	ErrBadCredentials = errors.New("email or password is incorrect")
	ErrUserDisabled   = errors.New("user is disabled")
	ErrNotAMember     = errors.New("user is not a member of that organization")
	ErrSessionInvalid = errors.New("refresh token is unknown, revoked or expired")
	ErrBadRole        = errors.New("role must be one of: owner, admin, member")
	ErrBadGlobalRole  = errors.New("role must be one of: user, admin")
	ErrWeakPassword   = errors.New("password must be at least 8 characters")
	ErrBadEmail       = errors.New("email is not valid")
	ErrBadSlug        = errors.New("slug must match [a-z][a-z0-9_-]{0,62}")
	ErrInsufficient   = errors.New("insufficient role")
)

// Global user roles and per-organization membership roles are separate: a
// platform admin is not automatically an owner of somebody's organization.
const (
	RoleUser  = "user"
	RoleAdmin = "admin"

	OrgOwner  = "owner"
	OrgAdmin  = "admin"
	OrgMember = "member"
)

var orgRoleRank = map[string]int{OrgMember: 1, OrgAdmin: 2, OrgOwner: 3}

func GlobalRoles() []string { return []string{RoleUser, RoleAdmin} }
func OrgRoles() []string    { return []string{OrgOwner, OrgAdmin, OrgMember} }

// Token lifetimes. The access token is short because it cannot be revoked;
// the refresh token is long because it can.
const (
	AccessTTL  = 15 * time.Minute
	RefreshTTL = 30 * 24 * time.Hour

	MinPasswordLen = 8
)

var (
	emailRe = regexp.MustCompile(`^[^@\s]+@[^@\s.]+\.[^@\s]+$`)
	slugRe  = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)
)

// User is an identity. PasswordHash is never populated on values returned to a
// caller; it exists only on the path between the database and bcrypt.
type User struct {
	ID           string `json:"id"`
	Email        string `json:"email"`
	Name         string `json:"name"`
	Role         string `json:"role"`
	Disabled     bool   `json:"disabled"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
	passwordHash string
}

// Org is a tenant.
type Org struct {
	ID        string `json:"id"`
	Slug      string `json:"slug"`
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"`
}

// Membership binds a user to an organization with a role.
type Membership struct {
	OrgID     string `json:"org_id"`
	OrgSlug   string `json:"org_slug"`
	UserID    string `json:"user_id"`
	UserEmail string `json:"user_email,omitempty"`
	Role      string `json:"role"`
	CreatedAt string `json:"created_at"`
}

// Tokens is what a successful login returns.
type Tokens struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	User         User   `json:"user"`
	Org          string `json:"org,omitempty"`
	OrgRole      string `json:"org_role,omitempty"`
}

// Auth owns identity storage and token issuance.
type Auth struct {
	db     *sql.DB
	kv     *kv.KV
	secret []byte
}

// New builds an Auth, resolving the token signing secret.
func New(db *sql.DB, k *kv.KV) (*Auth, error) {
	a := &Auth{db: db, kv: k}
	secret, err := a.resolveSecret()
	if err != nil {
		return nil, err
	}
	a.secret = secret
	return a, nil
}

const secretKey = "auth.jwt_secret"

// resolveSecret finds or creates the signing secret.
//
// BKN_AUTH_SECRET wins so a multi-instance deployment can share one. Otherwise
// it is generated once and kept in kv, encrypted when a keyring is configured.
// Rotating it invalidates every outstanding access token, which is the point.
func (a *Auth) resolveSecret() ([]byte, error) {
	if env := os.Getenv("BKN_AUTH_SECRET"); env != "" {
		return []byte(env), nil
	}
	if e, err := a.kv.Get(secretKey); err == nil && e.Value != "" {
		return []byte(e.Value), nil
	} else if err != nil && !errors.Is(err, kv.ErrNotFound) {
		return nil, err
	}

	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return nil, err
	}
	secret := hex.EncodeToString(buf)

	typ := kv.TypeEncrypted
	if _, err := a.kv.Set(secretKey, secret, typ, "token signing secret, generated on first use", false); err != nil {
		// No encryption key configured: store it as an opaque string rather
		// than refusing to have auth at all, but say so.
		fmt.Fprintf(os.Stderr,
			"[auth] no encryption key configured: storing the token signing secret unencrypted in %s\n"+
				"[auth] set BKN_ENCRYPTION_KEY, or BKN_AUTH_SECRET, to avoid this\n", secretKey)
		if _, err := a.kv.Set(secretKey, secret, kv.TypeString, "token signing secret, generated on first use", false); err != nil {
			return nil, err
		}
	}
	return []byte(secret), nil
}

// --- validation -----------------------------------------------------------

// NormalizeEmail is the single definition of what an email key looks like.
// Requirement R4: the Node backend lowercased in the server and again in every
// consumer, by convention, and the convention drifted.
func NormalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

func validateEmail(email string) (string, error) {
	n := NormalizeEmail(email)
	if !emailRe.MatchString(n) {
		return "", fmt.Errorf("%w: %q", ErrBadEmail, email)
	}
	return n, nil
}

func validateOrgRole(role string) error {
	if _, ok := orgRoleRank[role]; !ok {
		return fmt.Errorf("%w: got %q", ErrBadRole, role)
	}
	return nil
}

func validateGlobalRole(role string) error {
	if role != RoleUser && role != RoleAdmin {
		return fmt.Errorf("%w: got %q", ErrBadGlobalRole, role)
	}
	return nil
}

// AtLeast reports whether have satisfies the minimum want, using the
// owner > admin > member hierarchy.
func AtLeast(have, want string) bool {
	return orgRoleRank[have] >= orgRoleRank[want] && orgRoleRank[want] > 0
}

func isUnique(err error, column string) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE") && strings.Contains(err.Error(), column)
}

func now() string { return time.Now().UTC().Format(time.RFC3339) }

// --- users ----------------------------------------------------------------

// HashPassword hashes with bcrypt at the default cost.
//
// bcrypt, not something newer, because the Node backend used bcryptjs and its
// stored hashes must keep verifying: the $2a$/$2b$ format is the same one.
func HashPassword(plain string) (string, error) {
	if len(plain) < MinPasswordLen {
		return "", fmt.Errorf("%w: got %d characters", ErrWeakPassword, len(plain))
	}
	h, err := bcrypt.GenerateFromPassword([]byte(plain), bcrypt.DefaultCost)
	return string(h), err
}

// CreateUser registers a new identity.
func (a *Auth) CreateUser(email, password, name, role string) (User, error) {
	normalized, err := validateEmail(email)
	if err != nil {
		return User{}, err
	}
	if role == "" {
		role = RoleUser
	}
	if err := validateGlobalRole(role); err != nil {
		return User{}, err
	}
	hash, err := HashPassword(password)
	if err != nil {
		return User{}, err
	}

	u := User{
		ID: store.NewID(), Email: normalized, Name: name, Role: role,
		CreatedAt: now(), UpdatedAt: now(),
	}
	_, err = a.db.Exec(`
		INSERT INTO users (id, email, password_hash, name, role, disabled, created_at, updated_at)
		VALUES (?,?,?,?,?,0,?,?)`,
		u.ID, u.Email, hash, u.Name, u.Role, u.CreatedAt, u.UpdatedAt)
	if isUnique(err, "email") {
		return User{}, ErrEmailTaken
	}
	if err != nil {
		return User{}, err
	}
	return u, nil
}

const userCols = `id, email, password_hash, name, role, disabled, created_at, updated_at`

func scanUser(row interface{ Scan(...any) error }) (User, error) {
	var u User
	var disabled int
	err := row.Scan(&u.ID, &u.Email, &u.passwordHash, &u.Name, &u.Role, &disabled, &u.CreatedAt, &u.UpdatedAt)
	if err == sql.ErrNoRows {
		return User{}, ErrUserNotFound
	}
	if err != nil {
		return User{}, err
	}
	u.Disabled = disabled == 1
	return u, nil
}

// FindUser looks a user up by id or by email; the email is normalized first,
// so any spelling of the same address finds the same row.
func (a *Auth) FindUser(idOrEmail string) (User, error) {
	u, err := scanUser(a.db.QueryRow(`SELECT `+userCols+` FROM users WHERE id = ?`, idOrEmail))
	if err == nil {
		return u, nil
	}
	if !errors.Is(err, ErrUserNotFound) {
		return User{}, err
	}
	return scanUser(a.db.QueryRow(`SELECT `+userCols+` FROM users WHERE email = ?`, NormalizeEmail(idOrEmail)))
}

// ListUsers returns users, newest first.
func (a *Auth) ListUsers(limit, offset int) ([]User, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := a.db.Query(`SELECT `+userCols+` FROM users ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []User{}
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// UpdateUser changes only the fields whose pointers are non-nil.
func (a *Auth) UpdateUser(idOrEmail string, name, role, password *string, disabled *bool) (User, error) {
	u, err := a.FindUser(idOrEmail)
	if err != nil {
		return User{}, err
	}
	if name != nil {
		u.Name = *name
	}
	if role != nil {
		if err := validateGlobalRole(*role); err != nil {
			return User{}, err
		}
		u.Role = *role
	}
	if disabled != nil {
		u.Disabled = *disabled
	}
	hash := u.passwordHash
	if password != nil {
		hash, err = HashPassword(*password)
		if err != nil {
			return User{}, err
		}
	}
	u.UpdatedAt = now()

	dis := 0
	if u.Disabled {
		dis = 1
	}
	if _, err := a.db.Exec(`
		UPDATE users SET name=?, role=?, password_hash=?, disabled=?, updated_at=? WHERE id=?`,
		u.Name, u.Role, hash, dis, u.UpdatedAt, u.ID); err != nil {
		return User{}, err
	}
	// Changing a password or disabling an account must end existing sessions.
	if password != nil || (disabled != nil && *disabled) {
		if _, err := a.RevokeAllSessions(u.ID); err != nil {
			return User{}, err
		}
	}
	return u, nil
}

// DeleteUser removes a user, their memberships and their sessions.
func (a *Auth) DeleteUser(idOrEmail string) error {
	u, err := a.FindUser(idOrEmail)
	if err != nil {
		return err
	}
	for _, q := range []string{
		`DELETE FROM sessions WHERE user_id = ?`,
		`DELETE FROM memberships WHERE user_id = ?`,
		`DELETE FROM users WHERE id = ?`,
	} {
		if _, err := a.db.Exec(q, u.ID); err != nil {
			return err
		}
	}
	return nil
}
