package store

import (
	"context"
	"errors"
	"time"
)

// The second step of the login and the sessions. A person is named here by "source:lowercase name" (see UserKey), so that the local
// emergency account and a directory account of the same name are two people.

// Kinds of factor.
const (
	FactorTOTP    = "totp"
	FactorPasskey = "passkey"
)

// The most a person may have of each kind.
const MaxFactorsPerKind = 3

var ErrTooManyFactors = errors.New("this person has the most devices of this kind already: remove one first")

// Factor is one device of a person.
type Factor struct {
	ID        int64
	User      string
	Kind      string
	Name      string
	Secret    []byte // totp: the secret (decrypted here, never sent anywhere)
	CredID    []byte // passkey
	PublicKey []byte // passkey: the COSE key
	SignCount uint32
	LastStep  int64
	Created   time.Time
	LastUsed  time.Time
}

func (s *Store) Factors(ctx context.Context, user string) ([]Factor, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, user, kind, name, secret_enc, cred_id, public_key, sign_count, last_step, created, last_used
		FROM auth_factors WHERE user = ? ORDER BY id`, user)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Factor
	for rows.Next() {
		var f Factor
		var enc []byte
		var created, used int64
		var count int64
		if err := rows.Scan(&f.ID, &f.User, &f.Kind, &f.Name, &enc, &f.CredID, &f.PublicKey, &count, &f.LastStep, &created, &used); err != nil {
			return nil, err
		}
		f.SignCount = uint32(count)
		f.Created = time.Unix(created, 0)
		if used > 0 {
			f.LastUsed = time.Unix(used, 0)
		}
		if f.Kind == FactorTOTP {
			plain, err := s.secrets.Decrypt(enc)
			if err != nil {
				return nil, err
			}
			f.Secret = []byte(plain)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (s *Store) addFactor(ctx context.Context, f Factor, enc []byte) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM auth_factors WHERE user = ? AND kind = ?`, f.User, f.Kind).Scan(&n); err != nil {
		return 0, err
	}
	if n >= MaxFactorsPerKind {
		return 0, ErrTooManyFactors
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO auth_factors (user, kind, name, secret_enc, cred_id, public_key, sign_count, created) VALUES (?,?,?,?,?,?,?,?)`,
		f.User, f.Kind, f.Name, enc, f.CredID, f.PublicKey, f.SignCount, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	return id, tx.Commit()
}

// AddTOTP stores an authenticator app (its secret is encrypted with the key file, like the SNMP communities).
func (s *Store) AddTOTP(ctx context.Context, user, name string, secret []byte) (int64, error) {
	enc, err := s.secrets.Encrypt(string(secret))
	if err != nil {
		return 0, err
	}
	return s.addFactor(ctx, Factor{User: user, Kind: FactorTOTP, Name: name}, enc)
}

func (s *Store) AddPasskey(ctx context.Context, user, name string, credID, publicKey []byte, signCount uint32) (int64, error) {
	return s.addFactor(ctx, Factor{User: user, Kind: FactorPasskey, Name: name, CredID: credID, PublicKey: publicKey, SignCount: signCount}, nil)
}

// UseTOTPStep records the period of an accepted code. It is false when that period (or a later one) was used already: the check and the
// write are one statement, so that two requests with the same code cannot both pass.
func (s *Store) UseTOTPStep(ctx context.Context, id, step int64) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE auth_factors SET last_step = ?, last_used = ? WHERE id = ? AND kind = 'totp' AND last_step < ?`, step, time.Now().Unix(), id, step)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// UsePasskeyCount records the counter of an accepted signature (false when it did not go up, which is the same race).
func (s *Store) UsePasskeyCount(ctx context.Context, id int64, count uint32) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE auth_factors SET sign_count = ?, last_used = ? WHERE id = ? AND kind = 'passkey' AND (? = 0 OR sign_count < ?)`,
		count, time.Now().Unix(), id, count, count)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// RemoveFactor deletes one device of a person.
func (s *Store) RemoveFactor(ctx context.Context, user string, id int64) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM auth_factors WHERE id = ? AND user = ?`, id, user)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// ResetFactors removes everything of a person: the devices and the recovery codes (an administrator does it when somebody lost them all).
func (s *Store) ResetFactors(ctx context.Context, user string) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `DELETE FROM auth_factors WHERE user = ?`, user)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if _, err := tx.ExecContext(ctx, `DELETE FROM recovery_codes WHERE user = ?`, user); err != nil {
		return 0, err
	}
	return int(n), tx.Commit()
}

// FactorUser says how many devices a person has (for the list of the administrator).
type FactorUser struct {
	User     string `json:"user"`
	TOTP     int    `json:"totp"`
	Passkeys int    `json:"passkeys"`
	Recovery int    `json:"recovery"`
}

func (s *Store) UsersWithFactors(ctx context.Context) ([]FactorUser, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT user, SUM(kind = 'totp'), SUM(kind = 'passkey') FROM auth_factors GROUP BY user ORDER BY user`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FactorUser
	for rows.Next() {
		var u FactorUser
		if err := rows.Scan(&u.User, &u.TOTP, &u.Passkeys); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Recovery, _ = s.RecoveryLeft(ctx, out[i].User)
	}
	return out, nil
}

// ---- recovery codes (only their hashes are kept)

// ReplaceRecoveryCodes throws away the codes a person had and keeps these.
func (s *Store) ReplaceRecoveryCodes(ctx context.Context, user string, hashes []string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM recovery_codes WHERE user = ?`, user); err != nil {
		return err
	}
	for _, h := range hashes {
		if _, err := tx.ExecContext(ctx, `INSERT INTO recovery_codes (user, hash) VALUES (?,?)`, user, h); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// UseRecoveryCode deletes the code and says whether it was there: a code works once.
func (s *Store) UseRecoveryCode(ctx context.Context, user, hash string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM recovery_codes WHERE user = ? AND hash = ?`, user, hash)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

func (s *Store) RecoveryLeft(ctx context.Context, user string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM recovery_codes WHERE user = ?`, user).Scan(&n)
	return n, err
}

// ---- sessions

// StoredSession is a session as it is kept in the database.
type StoredSession struct {
	Key      string
	UserJSON string
	CSRF     string
	Created  time.Time
	LastSeen time.Time
	IP       string
	MFA      bool // the second step was passed
	Limited  bool // only the setting up of the second step may be done (the administrator made it a must and this person has none yet)
}

func (s *Store) SaveSession(ctx context.Context, x StoredSession) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO sessions (key, user_json, csrf, created, last_seen, ip, mfa, limited) VALUES (?,?,?,?,?,?,?,?)
		ON CONFLICT(key) DO UPDATE SET last_seen = excluded.last_seen, limited = excluded.limited, mfa = excluded.mfa`,
		x.Key, x.UserJSON, x.CSRF, x.Created.Unix(), x.LastSeen.Unix(), x.IP, x.MFA, x.Limited)
	return err
}

func (s *Store) TouchSession(ctx context.Context, key string, seen time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE sessions SET last_seen = ? WHERE key = ?`, seen.Unix(), key)
	return err
}

func (s *Store) DeleteSession(ctx context.Context, key string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE key = ?`, key)
	return err
}

// PruneSessions deletes the sessions that were last seen before the time.
func (s *Store) PruneSessions(ctx context.Context, before time.Time) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE last_seen < ?`, before.Unix())
	return err
}

// LoadSessions returns the sessions that were last seen after `since` (the rest are deleted).
func (s *Store) LoadSessions(ctx context.Context, since time.Time) ([]StoredSession, error) {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE last_seen < ?`, since.Unix()); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT key, user_json, csrf, created, last_seen, ip, mfa, limited FROM sessions`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StoredSession
	for rows.Next() {
		var x StoredSession
		var created, seen int64
		if err := rows.Scan(&x.Key, &x.UserJSON, &x.CSRF, &created, &seen, &x.IP, &x.MFA, &x.Limited); err != nil {
			return nil, err
		}
		x.Created, x.LastSeen = time.Unix(created, 0), time.Unix(seen, 0)
		out = append(out, x)
	}
	return out, rows.Err()
}
