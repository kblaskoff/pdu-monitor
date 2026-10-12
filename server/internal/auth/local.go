package auth

import (
	"context"
	"crypto/subtle"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

// LocalAdmin is an account that exists only in the configuration file, for the day the directory is down. It is off unless a bcrypt
// hash is configured (`pdumonitord hash-password`).
type LocalAdmin struct {
	Username string
	Hash     string
}

// a hash to compare against when the name does not match, so that the time does not tell
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("not the password"), bcrypt.DefaultCost)

func (l *LocalAdmin) Matches(username string) bool {
	return l != nil && l.Username != "" && strings.EqualFold(username, l.Username)
}

func (l *LocalAdmin) Authenticate(_ context.Context, username, password string) (*User, error) {
	nameOK := subtle.ConstantTimeCompare([]byte(strings.ToLower(username)), []byte(strings.ToLower(l.Username))) == 1
	hash := []byte(l.Hash)
	if !nameOK || l.Hash == "" {
		hash = dummyHash
	}
	err := bcrypt.CompareHashAndPassword(hash, []byte(password))
	if err != nil || !nameOK || l.Hash == "" {
		return nil, ErrBadCredentials
	}
	return &User{Name: l.Username, DisplayName: l.Username + " (emergency account)", Role: RoleAdmin, Source: "local"}, nil
}
