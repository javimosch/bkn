package auth

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/javimosch/bkn/internal/store"
)

// CreateOrg registers a tenant.
func (a *Auth) CreateOrg(slug, name string) (Org, error) {
	if !slugRe.MatchString(slug) {
		return Org{}, fmt.Errorf("%w: got %q", ErrBadSlug, slug)
	}
	if name == "" {
		name = slug
	}
	o := Org{ID: store.NewID(), Slug: slug, Name: name, CreatedAt: now()}
	_, err := a.db.Exec(`INSERT INTO orgs (id, slug, name, created_at) VALUES (?,?,?,?)`,
		o.ID, o.Slug, o.Name, o.CreatedAt)
	if isUnique(err, "slug") {
		return Org{}, ErrSlugTaken
	}
	if err != nil {
		return Org{}, err
	}
	return o, nil
}

// FindOrg looks an organization up by id or slug.
func (a *Auth) FindOrg(idOrSlug string) (Org, error) {
	var o Org
	err := a.db.QueryRow(`SELECT id, slug, name, created_at FROM orgs WHERE id = ? OR slug = ?`,
		idOrSlug, idOrSlug).Scan(&o.ID, &o.Slug, &o.Name, &o.CreatedAt)
	if err == sql.ErrNoRows {
		return Org{}, ErrOrgNotFound
	}
	return o, err
}

// ListOrgs returns every organization.
func (a *Auth) ListOrgs() ([]Org, error) {
	rows, err := a.db.Query(`SELECT id, slug, name, created_at FROM orgs ORDER BY slug`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Org{}
	for rows.Next() {
		var o Org
		if err := rows.Scan(&o.ID, &o.Slug, &o.Name, &o.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// DeleteOrg removes an organization and its memberships.
func (a *Auth) DeleteOrg(idOrSlug string) error {
	o, err := a.FindOrg(idOrSlug)
	if err != nil {
		return err
	}
	if _, err := a.db.Exec(`DELETE FROM memberships WHERE org_id = ?`, o.ID); err != nil {
		return err
	}
	_, err = a.db.Exec(`DELETE FROM orgs WHERE id = ?`, o.ID)
	return err
}

// AddMember adds or re-roles a user in an organization.
func (a *Auth) AddMember(orgIDOrSlug, userIDOrEmail, role string) (Membership, error) {
	if role == "" {
		role = OrgMember
	}
	if err := validateOrgRole(role); err != nil {
		return Membership{}, err
	}
	o, err := a.FindOrg(orgIDOrSlug)
	if err != nil {
		return Membership{}, err
	}
	u, err := a.FindUser(userIDOrEmail)
	if err != nil {
		return Membership{}, err
	}
	m := Membership{
		OrgID: o.ID, OrgSlug: o.Slug, UserID: u.ID, UserEmail: u.Email,
		Role: role, CreatedAt: now(),
	}
	_, err = a.db.Exec(`
		INSERT INTO memberships (org_id, user_id, role, created_at) VALUES (?,?,?,?)
		ON CONFLICT(org_id, user_id) DO UPDATE SET role = excluded.role`,
		m.OrgID, m.UserID, m.Role, m.CreatedAt)
	if err != nil {
		return Membership{}, err
	}
	return m, nil
}

// RemoveMember detaches a user from an organization.
func (a *Auth) RemoveMember(orgIDOrSlug, userIDOrEmail string) error {
	o, err := a.FindOrg(orgIDOrSlug)
	if err != nil {
		return err
	}
	u, err := a.FindUser(userIDOrEmail)
	if err != nil {
		return err
	}
	res, err := a.db.Exec(`DELETE FROM memberships WHERE org_id = ? AND user_id = ?`, o.ID, u.ID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotAMember
	}
	return nil
}

// Members lists an organization's members.
func (a *Auth) Members(orgIDOrSlug string) ([]Membership, error) {
	o, err := a.FindOrg(orgIDOrSlug)
	if err != nil {
		return nil, err
	}
	rows, err := a.db.Query(`
		SELECT m.user_id, u.email, m.role, m.created_at
		FROM memberships m JOIN users u ON u.id = m.user_id
		WHERE m.org_id = ? ORDER BY m.role, u.email`, o.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Membership{}
	for rows.Next() {
		m := Membership{OrgID: o.ID, OrgSlug: o.Slug}
		if err := rows.Scan(&m.UserID, &m.UserEmail, &m.Role, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// Memberships lists the organizations a user belongs to.
func (a *Auth) Memberships(userIDOrEmail string) ([]Membership, error) {
	u, err := a.FindUser(userIDOrEmail)
	if err != nil {
		return nil, err
	}
	rows, err := a.db.Query(`
		SELECT o.id, o.slug, m.role, m.created_at
		FROM memberships m JOIN orgs o ON o.id = m.org_id
		WHERE m.user_id = ? ORDER BY o.slug`, u.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Membership{}
	for rows.Next() {
		m := Membership{UserID: u.ID, UserEmail: u.Email}
		if err := rows.Scan(&m.OrgID, &m.OrgSlug, &m.Role, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// membershipRole returns a user's role in an organization, or ErrNotAMember.
func (a *Auth) membershipRole(orgID, userID string) (string, error) {
	var role string
	err := a.db.QueryRow(`SELECT role FROM memberships WHERE org_id = ? AND user_id = ?`,
		orgID, userID).Scan(&role)
	if err == sql.ErrNoRows {
		return "", ErrNotAMember
	}
	return role, err
}

// Can reports whether a user holds at least minRole in an organization.
// A platform admin is deliberately NOT granted org roles implicitly: being an
// operator of the deployment is not the same as being an owner of a tenant.
func (a *Auth) Can(userIDOrEmail, orgIDOrSlug, minRole string) (bool, error) {
	if err := validateOrgRole(minRole); err != nil {
		return false, err
	}
	o, err := a.FindOrg(orgIDOrSlug)
	if err != nil {
		return false, err
	}
	u, err := a.FindUser(userIDOrEmail)
	if err != nil {
		return false, err
	}
	role, err := a.membershipRole(o.ID, u.ID)
	if errors.Is(err, ErrNotAMember) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return AtLeast(role, minRole), nil
}
