// Package auth logs people in against LDAP (and an optional emergency account), decides their role from their groups, and keeps
// the sessions.
package auth

import (
	"context"
	"errors"
	"strings"
)

// Role of a person, from the highest to the lowest.
type Role string

const (
	RoleAdmin    Role = "admin"    // everything, including settings
	RoleOperator Role = "operator" // may switch power
	RoleViewer   Role = "viewer"   // only looks
)

func (r Role) Rank() int {
	switch r {
	case RoleAdmin:
		return 3
	case RoleOperator:
		return 2
	case RoleViewer:
		return 1
	}
	return 0
}

// Can says whether the role is at least `needed`.
func (r Role) Can(needed Role) bool { return r.Rank() >= needed.Rank() && needed.Rank() > 0 }

// User is a person who has logged in.
type User struct {
	Name        string   `json:"name"`
	DisplayName string   `json:"displayName"`
	Role        Role     `json:"role"`
	Groups      []string `json:"groups,omitempty"`
	Source      string   `json:"source"` // "ldap" or "local"
}

var (
	// ErrBadCredentials: wrong user name or password (the same answer for both, so that nobody can find out which names exist).
	ErrBadCredentials = errors.New("wrong user name or password")
	// ErrNotAllowed: the password is right but the person is in none of the groups that may use the application.
	ErrNotAllowed = errors.New("this account is not in a group that may use this application")
	// ErrUnavailable: the directory could not be reached or answered something unexpected.
	ErrUnavailable = errors.New("the login service is not available")
)

// Authenticator checks a user name and a password.
type Authenticator interface {
	Authenticate(ctx context.Context, username, password string) (*User, error)
}

// ValidUsername rejects what cannot be a user name before it goes anywhere near a directory.
func ValidUsername(u string) bool {
	if u == "" || len(u) > 128 {
		return false
	}
	for _, r := range u {
		if r < ' ' || r == 0x7f {
			return false
		}
	}
	return strings.TrimSpace(u) == u
}

// Chain tries the emergency account first when the name matches, otherwise the directory.
type Chain struct {
	Local *LocalAdmin
	LDAP  Authenticator
}

func (c *Chain) Authenticate(ctx context.Context, username, password string) (*User, error) {
	if !ValidUsername(username) || password == "" {
		return nil, ErrBadCredentials
	}
	if c.Local != nil && c.Local.Matches(username) {
		return c.Local.Authenticate(ctx, username, password)
	}
	if c.LDAP == nil {
		return nil, ErrUnavailable
	}
	return c.LDAP.Authenticate(ctx, username, password)
}
