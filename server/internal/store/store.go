// Package store keeps racks, PDUs, hosts, the audit log and the history in one SQLite file.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/kblaskoff/pdu-monitor/server/internal/pdu"
)

// Store is the database.
type Store struct {
	db      *sql.DB
	secrets *Secrets
}

var migrations = []string{
	`CREATE TABLE racks (
		id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE, max_amps REAL NOT NULL DEFAULT 24, sort INTEGER NOT NULL DEFAULT 0);
	 CREATE TABLE pdus (
		id INTEGER PRIMARY KEY, rack_id INTEGER NOT NULL REFERENCES racks(id) ON DELETE CASCADE, name TEXT NOT NULL UNIQUE,
		vendor TEXT NOT NULL, host TEXT NOT NULL, port INTEGER NOT NULL DEFAULT 161, max_amps REAL NOT NULL DEFAULT 24,
		bank_max_amps REAL, read_enc BLOB, write_enc BLOB);
	 CREATE TABLE hosts (
		id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE, address TEXT NOT NULL, ssh_port INTEGER NOT NULL DEFAULT 22,
		ssh_user TEXT NOT NULL DEFAULT 'hostinfo-ro', rack_id INTEGER REFERENCES racks(id) ON DELETE SET NULL,
		outlet_key TEXT NOT NULL DEFAULT '', host_key TEXT NOT NULL DEFAULT '', enabled INTEGER NOT NULL DEFAULT 1);
	 CREATE TABLE labels (rack_id INTEGER NOT NULL REFERENCES racks(id) ON DELETE CASCADE, key TEXT NOT NULL, label TEXT NOT NULL, PRIMARY KEY (rack_id, key));
	 CREATE TABLE audit (id INTEGER PRIMARY KEY, ts INTEGER NOT NULL, user TEXT NOT NULL, action TEXT NOT NULL, detail TEXT NOT NULL, ok INTEGER NOT NULL);
	 CREATE INDEX audit_ts ON audit(ts);
	 CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
	 CREATE TABLE pdu_samples (pdu_id INTEGER NOT NULL REFERENCES pdus(id) ON DELETE CASCADE, ts INTEGER NOT NULL, amps REAL, watts REAL, PRIMARY KEY (pdu_id, ts));`,
	// 2: hosts can be read by a collector (a separate process that pushes the readings) instead of by this server
	`ALTER TABLE hosts ADD COLUMN collector TEXT NOT NULL DEFAULT '';`,
	// 3: the CPU idle of the hosts, one row per host and hour (sum and count of the minute readings, and the lowest reading): a year of 20
	// hosts is about 175 000 rows. Kept on the server, so that it does not depend on how long an agent keeps its own files.
	`CREATE TABLE host_idle (host_id INTEGER NOT NULL REFERENCES hosts(id) ON DELETE CASCADE, hour INTEGER NOT NULL,
		sum REAL NOT NULL, n INTEGER NOT NULL, min REAL NOT NULL, PRIMARY KEY (host_id, hour));`,
	// 4: the second step of the login (authenticator apps and passkeys), the recovery codes, and the sessions (kept here so that a restart
	// of the server, which an update makes, does not log everybody out). Only the hash of a session token is stored, as in memory.
	`CREATE TABLE auth_factors (id INTEGER PRIMARY KEY, user TEXT NOT NULL, kind TEXT NOT NULL, name TEXT NOT NULL,
		secret_enc BLOB, cred_id BLOB, public_key BLOB, sign_count INTEGER NOT NULL DEFAULT 0, last_step INTEGER NOT NULL DEFAULT 0,
		created INTEGER NOT NULL, last_used INTEGER NOT NULL DEFAULT 0);
	 CREATE INDEX auth_factors_user ON auth_factors(user);
	 CREATE UNIQUE INDEX auth_factors_cred ON auth_factors(cred_id) WHERE cred_id IS NOT NULL;
	 CREATE TABLE recovery_codes (user TEXT NOT NULL, hash TEXT NOT NULL, PRIMARY KEY (user, hash));
	 CREATE TABLE sessions (key TEXT PRIMARY KEY, user_json TEXT NOT NULL, csrf TEXT NOT NULL, created INTEGER NOT NULL,
		last_seen INTEGER NOT NULL, ip TEXT NOT NULL, mfa INTEGER NOT NULL DEFAULT 0, limited INTEGER NOT NULL DEFAULT 0);`,
	// 5: how a person likes to see things (per person, not for everybody): for now how CPU idle is shown
	`CREATE TABLE user_prefs (user TEXT NOT NULL, key TEXT NOT NULL, value TEXT NOT NULL, PRIMARY KEY (user, key));`,
}

// Open opens (and creates) the database file. secrets may be nil only in tests that never store communities.
func Open(path string, secrets *Secrets) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite writes one at a time; this keeps it simple and safe
	s := &Store{db: db, secrets: secrets}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL)`); err != nil {
		return err
	}
	var version int
	err := s.db.QueryRow(`SELECT version FROM schema_version`).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := s.db.Exec(`INSERT INTO schema_version VALUES (0)`); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	for i := version; i < len(migrations); i++ {
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.Exec(`UPDATE schema_version SET version = ?`, i+1); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// ---- racks

type Rack struct {
	ID      int64   `json:"id"`
	Name    string  `json:"name"`
	MaxAmps float64 `json:"maxAmps"`
	Sort    int     `json:"sort"`
}

func (s *Store) Racks(ctx context.Context) ([]Rack, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, max_amps, sort FROM racks ORDER BY sort, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Rack
	for rows.Next() {
		var r Rack
		if err := rows.Scan(&r.ID, &r.Name, &r.MaxAmps, &r.Sort); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func validName(name string) error {
	n := strings.TrimSpace(name)
	if n == "" || len(n) > 100 {
		return errors.New("the name must have 1 to 100 characters")
	}
	return nil
}

func (s *Store) SaveRack(ctx context.Context, r Rack) (Rack, error) {
	r.Name = strings.TrimSpace(r.Name)
	if err := validName(r.Name); err != nil {
		return r, err
	}
	if r.MaxAmps <= 0 || r.MaxAmps > 10000 {
		return r, errors.New("the limit must be more than 0 A")
	}
	if r.ID == 0 {
		res, err := s.db.ExecContext(ctx, `INSERT INTO racks (name, max_amps, sort) VALUES (?, ?, ?)`, r.Name, r.MaxAmps, r.Sort)
		if err != nil {
			return r, friendly(err)
		}
		r.ID, _ = res.LastInsertId()
		return r, nil
	}
	res, err := s.db.ExecContext(ctx, `UPDATE racks SET name = ?, max_amps = ?, sort = ? WHERE id = ?`, r.Name, r.MaxAmps, r.Sort, r.ID)
	if err != nil {
		return r, friendly(err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return r, ErrNotFound
	}
	return r, nil
}

func (s *Store) DeleteRack(ctx context.Context, id int64) error {
	return s.deleteByID(ctx, "racks", id)
}

// ErrNotFound is returned when an id does not exist.
var ErrNotFound = errors.New("not found")

func friendly(err error) error {
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return errors.New("this name is already used")
	}
	return err
}

func (s *Store) deleteByID(ctx context.Context, table string, id int64) error {
	res, err := s.db.ExecContext(ctx, "DELETE FROM "+table+" WHERE id = ?", id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ---- PDUs

type PDU struct {
	ID          int64      `json:"id"`
	RackID      int64      `json:"rackId"`
	Name        string     `json:"name"`
	Vendor      pdu.Vendor `json:"vendor"`
	Host        string     `json:"host"`
	Port        int        `json:"port"`
	MaxAmps     float64    `json:"maxAmps"`
	BankMaxAmps *float64   `json:"bankMaxAmps"`
	HasRead     bool       `json:"hasReadCommunity"`
	HasWrite    bool       `json:"hasWriteCommunity"`
}

// Communities are the SNMP secrets of a PDU. Empty means "not set" on save: the stored one is kept.
type Communities struct{ Read, Write string }

const pduColumns = `id, rack_id, name, vendor, host, port, max_amps, bank_max_amps, length(read_enc) > 0, length(write_enc) > 0`

func scanPDU(sc interface{ Scan(...any) error }) (PDU, error) {
	var p PDU
	var bank sql.NullFloat64
	var vendor string
	var hasRead, hasWrite sql.NullBool
	if err := sc.Scan(&p.ID, &p.RackID, &p.Name, &vendor, &p.Host, &p.Port, &p.MaxAmps, &bank, &hasRead, &hasWrite); err != nil {
		return p, err
	}
	p.Vendor = pdu.Vendor(vendor)
	if bank.Valid {
		p.BankMaxAmps = &bank.Float64
	}
	p.HasRead, p.HasWrite = hasRead.Bool, hasWrite.Bool
	return p, nil
}

func (s *Store) PDUs(ctx context.Context) ([]PDU, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+pduColumns+` FROM pdus ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PDU
	for rows.Next() {
		p, err := scanPDU(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) PDU(ctx context.Context, id int64) (PDU, error) {
	p, err := scanPDU(s.db.QueryRowContext(ctx, `SELECT `+pduColumns+` FROM pdus WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrNotFound
	}
	return p, err
}

// PDUCommunities returns the decrypted communities (only for the poller; they never go to a browser).
func (s *Store) PDUCommunities(ctx context.Context, id int64) (Communities, error) {
	var r, w []byte
	err := s.db.QueryRowContext(ctx, `SELECT read_enc, write_enc FROM pdus WHERE id = ?`, id).Scan(&r, &w)
	if errors.Is(err, sql.ErrNoRows) {
		return Communities{}, ErrNotFound
	}
	if err != nil {
		return Communities{}, err
	}
	var c Communities
	if c.Read, err = s.secrets.Decrypt(r); err != nil {
		return c, err
	}
	c.Write, err = s.secrets.Decrypt(w)
	return c, err
}

// SavePDU inserts (ID 0) or updates a PDU. Empty communities keep what is stored; to remove the write community use ClearWrite.
func (s *Store) SavePDU(ctx context.Context, p PDU, c Communities, clearWrite bool) (PDU, error) {
	p.Name, p.Host = strings.TrimSpace(p.Name), strings.TrimSpace(p.Host)
	if err := validName(p.Name); err != nil {
		return p, err
	}
	if !p.Vendor.Valid() {
		return p, errors.New("choose CyberPower or APC")
	}
	if p.Host == "" || len(p.Host) > 255 || strings.ContainsAny(p.Host, " \t\r\n/") {
		return p, errors.New("enter the address of the PDU")
	}
	if p.Port <= 0 || p.Port > 65535 {
		return p, errors.New("the port must be between 1 and 65535")
	}
	if p.MaxAmps <= 0 || p.MaxAmps > 10000 {
		return p, errors.New("the limit must be more than 0 A")
	}
	if p.BankMaxAmps != nil && *p.BankMaxAmps <= 0 {
		p.BankMaxAmps = nil
	}
	readEnc, err := s.secrets.Encrypt(c.Read)
	if err != nil {
		return p, err
	}
	writeEnc, err := s.secrets.Encrypt(c.Write)
	if err != nil {
		return p, err
	}
	if p.ID == 0 {
		res, err := s.db.ExecContext(ctx, `INSERT INTO pdus (rack_id, name, vendor, host, port, max_amps, bank_max_amps, read_enc, write_enc) VALUES (?,?,?,?,?,?,?,?,?)`,
			p.RackID, p.Name, string(p.Vendor), p.Host, p.Port, p.MaxAmps, p.BankMaxAmps, readEnc, writeEnc)
		if err != nil {
			return p, friendly(err)
		}
		p.ID, _ = res.LastInsertId()
		return s.PDU(ctx, p.ID)
	}
	res, err := s.db.ExecContext(ctx, `UPDATE pdus SET rack_id=?, name=?, vendor=?, host=?, port=?, max_amps=?, bank_max_amps=?,
		read_enc = CASE WHEN ? THEN ? ELSE read_enc END, write_enc = CASE WHEN ? THEN NULL WHEN ? THEN ? ELSE write_enc END WHERE id = ?`,
		p.RackID, p.Name, string(p.Vendor), p.Host, p.Port, p.MaxAmps, p.BankMaxAmps,
		c.Read != "", readEnc, clearWrite, c.Write != "", writeEnc, p.ID)
	if err != nil {
		return p, friendly(err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return p, ErrNotFound
	}
	return s.PDU(ctx, p.ID)
}

func (s *Store) DeletePDU(ctx context.Context, id int64) error { return s.deleteByID(ctx, "pdus", id) }

// ---- hosts

type Host struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Address   string `json:"address"`
	Port      int    `json:"port"`
	User      string `json:"user"`
	RackID    *int64 `json:"rackId"`
	OutletKey string `json:"outletKey"`
	// HostKey is the public key of the host that was confirmed by an administrator ("" until then).
	HostKey string `json:"hostKey"`
	Enabled bool   `json:"enabled"`
	// Collector is the name of the collector that reads this host and pushes the readings ("" = this server reads it over SSH).
	Collector string `json:"collector"`
}

const hostColumns = `id, name, address, ssh_port, ssh_user, rack_id, outlet_key, host_key, enabled, collector`

func scanHost(sc interface{ Scan(...any) error }) (Host, error) {
	var h Host
	var rack sql.NullInt64
	if err := sc.Scan(&h.ID, &h.Name, &h.Address, &h.Port, &h.User, &rack, &h.OutletKey, &h.HostKey, &h.Enabled, &h.Collector); err != nil {
		return h, err
	}
	if rack.Valid {
		h.RackID = &rack.Int64
	}
	return h, nil
}

func (s *Store) Hosts(ctx context.Context) ([]Host, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+hostColumns+` FROM hosts ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Host
	for rows.Next() {
		h, err := scanHost(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func (s *Store) Host(ctx context.Context, id int64) (Host, error) {
	h, err := scanHost(s.db.QueryRowContext(ctx, `SELECT `+hostColumns+` FROM hosts WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return h, ErrNotFound
	}
	return h, err
}

// SaveHost inserts (ID 0) or updates a host. The trusted key is only changed through TrustHostKey. A host read by a collector needs
// no address: the collector knows how to reach it.
func (s *Store) SaveHost(ctx context.Context, h Host) (Host, error) {
	h.Name, h.Address, h.User = strings.TrimSpace(h.Name), strings.TrimSpace(h.Address), strings.TrimSpace(h.User)
	if err := validName(h.Name); err != nil {
		return h, err
	}
	if h.Collector == "" {
		if h.Address == "" || len(h.Address) > 255 || strings.ContainsAny(h.Address, " \t\r\n/@") {
			return h, errors.New("enter the address of the host")
		}
		if h.Port <= 0 || h.Port > 65535 {
			return h, errors.New("the port must be between 1 and 65535")
		}
		if h.User == "" {
			h.User = "hostinfo-ro"
		}
		if strings.ContainsAny(h.User, " \t\r\n/@:") {
			return h, errors.New("bad user name")
		}
	} else {
		if h.Port == 0 {
			h.Port = 22
		}
		if h.User == "" {
			h.User = "hostinfo-ro"
		}
	}
	h.OutletKey = strings.TrimSpace(h.OutletKey)
	if h.ID == 0 {
		res, err := s.db.ExecContext(ctx, `INSERT INTO hosts (name, address, ssh_port, ssh_user, rack_id, outlet_key, enabled, collector) VALUES (?,?,?,?,?,?,?,?)`,
			h.Name, h.Address, h.Port, h.User, h.RackID, h.OutletKey, h.Enabled, h.Collector)
		if err != nil {
			return h, friendly(err)
		}
		h.ID, _ = res.LastInsertId()
		return s.Host(ctx, h.ID)
	}
	// a changed address or port needs the key to be confirmed again; who reads the host is not changed here
	old, err := s.Host(ctx, h.ID)
	if err != nil {
		return h, err
	}
	key := old.HostKey
	if old.Address != h.Address || old.Port != h.Port {
		key = ""
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE hosts SET name=?, address=?, ssh_port=?, ssh_user=?, rack_id=?, outlet_key=?, enabled=?, host_key=? WHERE id=?`,
		h.Name, h.Address, h.Port, h.User, h.RackID, h.OutletKey, h.Enabled, key, h.ID); err != nil {
		return h, friendly(err)
	}
	return s.Host(ctx, h.ID)
}

// ErrOtherCollector: the host exists and is read by this server or by another collector.
var ErrOtherCollector = errors.New("this host is defined on the server or belongs to another collector")

// CollectedHost returns the host of this name that the collector reports, and creates it the first time (without a rack: an
// administrator links it to a rack and a power outlet name afterwards). The second result says whether it was created.
func (s *Store) CollectedHost(ctx context.Context, collector, name string) (Host, bool, error) {
	name = strings.TrimSpace(name)
	if err := validName(name); err != nil {
		return Host{}, false, err
	}
	hosts, err := s.Hosts(ctx)
	if err != nil {
		return Host{}, false, err
	}
	for _, h := range hosts {
		if h.Name == name {
			if h.Collector != collector {
				return h, false, ErrOtherCollector
			}
			return h, false, nil
		}
	}
	h, err := s.SaveHost(ctx, Host{Name: name, Collector: collector, Enabled: true})
	return h, err == nil, err
}

// TrustHostKey stores the key an administrator has confirmed (authorized_keys format, e.g. "ssh-ed25519 AAAA...").
func (s *Store) TrustHostKey(ctx context.Context, id int64, key string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE hosts SET host_key = ? WHERE id = ?`, strings.TrimSpace(key), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) DeleteHost(ctx context.Context, id int64) error {
	return s.deleteByID(ctx, "hosts", id)
}

// ---- labels (the names of servers typed in the Mac application)

func (s *Store) Labels(ctx context.Context) (map[int64]map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT rack_id, key, label FROM labels`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]map[string]string{}
	for rows.Next() {
		var rack int64
		var key, label string
		if err := rows.Scan(&rack, &key, &label); err != nil {
			return nil, err
		}
		if out[rack] == nil {
			out[rack] = map[string]string{}
		}
		out[rack][key] = label
	}
	return out, rows.Err()
}

func (s *Store) SetLabel(ctx context.Context, rack int64, key, label string) error {
	key, label = pdu.ServerKey(key), strings.TrimSpace(label)
	if label == "" {
		_, err := s.db.ExecContext(ctx, `DELETE FROM labels WHERE rack_id = ? AND key = ?`, rack, key)
		return err
	}
	if len(label) > 100 {
		return errors.New("the name is too long")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO labels (rack_id, key, label) VALUES (?,?,?) ON CONFLICT(rack_id, key) DO UPDATE SET label = excluded.label`, rack, key, label)
	return err
}

// ---- settings

// Settings of the application, edited by an administrator.
type Settings struct {
	PollSeconds        int `json:"pollSeconds"`
	HostPollSeconds    int `json:"hostPollSeconds"`
	WarnPercent        int `json:"warnPercent"`
	RestartDelaySecond int `json:"restartDelaySeconds"`
	SampleRetentionDay int `json:"sampleRetentionDays"`
	// The names of the outlets are taken from servman and written to the PDUs (see the servman package).
	ServmanPollMinutes int    `json:"servmanPollMinutes"` // how often servman is asked and the plan made; 0 = never by itself
	ServmanSyncMinutes int    `json:"servmanSyncMinutes"` // how often the planned names are written to the PDUs; 0 = only when an administrator presses the button
	ServmanNameFormat  string `json:"servmanNameFormat"`  // "ccid" (200U13) or "ccid_name" (200U13 a42sf)
	// Login: how long a person stays logged in (without and with using the application), and whether the second step is a must.
	SessionHours int  `json:"sessionHours"`
	RequireMFA   bool `json:"requireMfa"` // everybody must have an authenticator app or a passkey (the emergency local account is not forced)
}

// The two forms of an outlet name.
const (
	NameCCID     = "ccid"
	NameCCIDName = "ccid_name"
)

func DefaultSettings() Settings {
	return Settings{PollSeconds: 10, HostPollSeconds: 60, WarnPercent: 80, RestartDelaySecond: 8, SampleRetentionDay: 90,
		ServmanPollMinutes: 15, ServmanSyncMinutes: 0, ServmanNameFormat: NameCCID, SessionHours: 72}
}

func (s *Store) Settings(ctx context.Context) (Settings, error) {
	st := DefaultSettings()
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = 'app'`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		return DefaultSettings(), nil
	}
	return st.sane(), nil
}

func (st Settings) sane() Settings {
	d := DefaultSettings()
	clamp := func(v, lo, hi, def int) int {
		if v < lo || v > hi {
			return def
		}
		return v
	}
	st.PollSeconds = clamp(st.PollSeconds, 3, 600, d.PollSeconds)
	st.HostPollSeconds = clamp(st.HostPollSeconds, 15, 3600, d.HostPollSeconds)
	st.WarnPercent = clamp(st.WarnPercent, 50, 99, d.WarnPercent)
	st.RestartDelaySecond = clamp(st.RestartDelaySecond, 3, 60, d.RestartDelaySecond)
	st.SampleRetentionDay = clamp(st.SampleRetentionDay, 1, 3650, d.SampleRetentionDay)
	st.ServmanPollMinutes = clamp(st.ServmanPollMinutes, 0, 1440, d.ServmanPollMinutes)
	st.ServmanSyncMinutes = clamp(st.ServmanSyncMinutes, 0, 10080, d.ServmanSyncMinutes)
	st.SessionHours = clamp(st.SessionHours, 1, 720, d.SessionHours)
	if st.ServmanNameFormat != NameCCID && st.ServmanNameFormat != NameCCIDName {
		st.ServmanNameFormat = d.ServmanNameFormat
	}
	return st
}

func (s *Store) SaveSettings(ctx context.Context, st Settings) (Settings, error) {
	st = st.sane()
	raw, _ := json.Marshal(st)
	_, err := s.db.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES ('app', ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, string(raw))
	return st, err
}

// ---- audit

type AuditEntry struct {
	ID     int64     `json:"id"`
	Time   time.Time `json:"time"`
	User   string    `json:"user"`
	Action string    `json:"action"`
	Detail string    `json:"detail"`
	OK     bool      `json:"ok"`
}

// Audit writes one line of the log of who did what.
func (s *Store) Audit(ctx context.Context, user, action, detail string, ok bool) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO audit (ts, user, action, detail, ok) VALUES (?,?,?,?,?)`, time.Now().Unix(), user, action, detail, ok)
	return err
}

// AuditLog returns the newest entries first.
func (s *Store) AuditLog(ctx context.Context, limit int) ([]AuditEntry, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, ts, user, action, detail, ok FROM audit ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		var ts int64
		if err := rows.Scan(&e.ID, &ts, &e.User, &e.Action, &e.Detail, &e.OK); err != nil {
			return nil, err
		}
		e.Time = time.Unix(ts, 0)
		out = append(out, e)
	}
	return out, rows.Err()
}

// ---- history of the PDUs

type Sample struct {
	PDUID int64
	Time  time.Time
	Amps  *float64
	Watts *float64
}

func (s *Store) AddSamples(ctx context.Context, samples []Sample) error {
	if len(samples) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, sm := range samples {
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO pdu_samples (pdu_id, ts, amps, watts) VALUES (?,?,?,?)`, sm.PDUID, sm.Time.Unix(), sm.Amps, sm.Watts); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Samples of one PDU since a time, oldest first.
func (s *Store) Samples(ctx context.Context, pduID int64, since time.Time) ([]Sample, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT ts, amps, watts FROM pdu_samples WHERE pdu_id = ? AND ts >= ? ORDER BY ts`, pduID, since.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Sample
	for rows.Next() {
		var ts int64
		var a, w sql.NullFloat64
		if err := rows.Scan(&ts, &a, &w); err != nil {
			return nil, err
		}
		sm := Sample{PDUID: pduID, Time: time.Unix(ts, 0)}
		if a.Valid {
			sm.Amps = &a.Float64
		}
		if w.Valid {
			sm.Watts = &w.Float64
		}
		out = append(out, sm)
	}
	return out, rows.Err()
}

// PruneSamples removes what is older than the retention. It returns how many rows were removed.
func (s *Store) PruneSamples(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM pdu_samples WHERE ts < ?`, before.Unix())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
