package store_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/kblaskoff/pdu-monitor/server/internal/pdu"
	"github.com/kblaskoff/pdu-monitor/server/internal/store"
)

func open(t *testing.T) (*store.Store, string) {
	t.Helper()
	dir := t.TempDir()
	key, err := store.LoadOrCreateKey(filepath.Join(dir, "secret.key"))
	if err != nil {
		t.Fatal(err)
	}
	sec, err := store.NewSecrets(key)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "db.sqlite")
	s, err := store.Open(path, sec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

func TestSecretsRoundTripAndKeyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "k")
	key, err := store.LoadOrCreateKey(path)
	if err != nil || len(key) != 32 {
		t.Fatal(err, len(key))
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Errorf("key file mode %v", st.Mode().Perm())
	}
	again, _ := store.LoadOrCreateKey(path)
	if !bytes.Equal(key, again) {
		t.Fatal("the key must stay the same")
	}
	os.WriteFile(path, []byte("short"), 0o600)
	if _, err := store.LoadOrCreateKey(path); err == nil {
		t.Fatal("a short key file must be refused")
	}
	sec, _ := store.NewSecrets(key)
	blob, _ := sec.Encrypt("s3cret")
	if bytes.Contains(blob, []byte("s3cret")) {
		t.Fatal("not encrypted")
	}
	if plain, err := sec.Decrypt(blob); err != nil || plain != "s3cret" {
		t.Fatal(plain, err)
	}
	other, _ := store.NewSecrets(bytes.Repeat([]byte{7}, 32))
	if _, err := other.Decrypt(blob); err == nil {
		t.Fatal("the wrong key must not decrypt")
	}
	blob[len(blob)-1] ^= 1
	if _, err := sec.Decrypt(blob); err == nil {
		t.Fatal("damaged data must not decrypt")
	}
}

func TestRacksPDUsAndTheCommunitiesAreNotInTheFile(t *testing.T) {
	s, path := open(t)
	ctx := context.Background()
	rack, err := s.SaveRack(ctx, store.Rack{Name: "200", MaxAmps: 24})
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.SavePDU(ctx, store.PDU{RackID: rack.ID, Name: "P200A", Vendor: pdu.APC, Host: "10.0.0.5", Port: 161, MaxAmps: 24},
		store.Communities{Read: "very-secret-read", Write: "very-secret-write"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !p.HasRead || !p.HasWrite {
		t.Errorf("%+v", p)
	}
	c, err := s.PDUCommunities(ctx, p.ID)
	if err != nil || c.Read != "very-secret-read" || c.Write != "very-secret-write" {
		t.Fatalf("%+v %v", c, err)
	}
	s.Close()
	raw, _ := os.ReadFile(path)
	wal, _ := os.ReadFile(path + "-wal")
	if bytes.Contains(raw, []byte("very-secret")) || bytes.Contains(wal, []byte("very-secret")) {
		t.Fatal("a community is readable in the database file")
	}
}

func TestSavingAPDUKeepsTheCommunitiesWhenEmptyAndCanClearTheWriteOne(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	rack, _ := s.SaveRack(ctx, store.Rack{Name: "r", MaxAmps: 24})
	p, _ := s.SavePDU(ctx, store.PDU{RackID: rack.ID, Name: "A", Vendor: pdu.CyberPower, Host: "h", Port: 161, MaxAmps: 24}, store.Communities{Read: "r1", Write: "w1"}, false)
	p.MaxAmps = 16
	if _, err := s.SavePDU(ctx, p, store.Communities{}, false); err != nil {
		t.Fatal(err)
	}
	c, _ := s.PDUCommunities(ctx, p.ID)
	if c.Read != "r1" || c.Write != "w1" {
		t.Fatalf("the communities were lost: %+v", c)
	}
	if _, err := s.SavePDU(ctx, p, store.Communities{Read: "r2"}, true); err != nil {
		t.Fatal(err)
	}
	c, _ = s.PDUCommunities(ctx, p.ID)
	if c.Read != "r2" || c.Write != "" {
		t.Fatalf("%+v", c)
	}
	got, _ := s.PDU(ctx, p.ID)
	if got.MaxAmps != 16 || got.HasWrite {
		t.Fatalf("%+v", got)
	}
}

func TestValidation(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	if _, err := s.SaveRack(ctx, store.Rack{Name: " ", MaxAmps: 24}); err == nil {
		t.Error("empty rack name")
	}
	if _, err := s.SaveRack(ctx, store.Rack{Name: "x", MaxAmps: 0}); err == nil {
		t.Error("zero limit")
	}
	rack, _ := s.SaveRack(ctx, store.Rack{Name: "x", MaxAmps: 24})
	if _, err := s.SaveRack(ctx, store.Rack{Name: "x", MaxAmps: 24}); err == nil || !strings.Contains(err.Error(), "already used") {
		t.Errorf("duplicate: %v", err)
	}
	bad := []store.PDU{
		{RackID: rack.ID, Name: "a", Vendor: "ibm", Host: "h", Port: 161, MaxAmps: 24},
		{RackID: rack.ID, Name: "a", Vendor: pdu.APC, Host: "", Port: 161, MaxAmps: 24},
		{RackID: rack.ID, Name: "a", Vendor: pdu.APC, Host: "a b", Port: 161, MaxAmps: 24},
		{RackID: rack.ID, Name: "a", Vendor: pdu.APC, Host: "h", Port: 0, MaxAmps: 24},
		{RackID: rack.ID, Name: "a", Vendor: pdu.APC, Host: "h", Port: 70000, MaxAmps: 24},
		{RackID: rack.ID, Name: "a", Vendor: pdu.APC, Host: "h", Port: 161, MaxAmps: 0},
		{RackID: 999, Name: "a", Vendor: pdu.APC, Host: "h", Port: 161, MaxAmps: 24}, // no such rack
	}
	for i, p := range bad {
		if _, err := s.SavePDU(ctx, p, store.Communities{Read: "x"}, false); err == nil {
			t.Errorf("case %d accepted", i)
		}
	}
}

func TestDeletingARackRemovesItsPDUsButKeepsItsHosts(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	rack, _ := s.SaveRack(ctx, store.Rack{Name: "r", MaxAmps: 24})
	p, _ := s.SavePDU(ctx, store.PDU{RackID: rack.ID, Name: "A", Vendor: pdu.APC, Host: "h", Port: 161, MaxAmps: 24}, store.Communities{Read: "x"}, false)
	h, _ := s.SaveHost(ctx, store.Host{Name: "a52sf", Address: "10.0.0.9", Port: 22, RackID: &rack.ID, Enabled: true})
	_ = s.AddSamples(ctx, []store.Sample{{PDUID: p.ID, Time: time.Now()}})
	if err := s.DeleteRack(ctx, rack.ID); err != nil {
		t.Fatal(err)
	}
	if pdus, _ := s.PDUs(ctx); len(pdus) != 0 {
		t.Error("the PDUs must go with the rack")
	}
	got, err := s.Host(ctx, h.ID)
	if err != nil || got.RackID != nil {
		t.Errorf("the host stays, without a rack: %+v %v", got, err)
	}
	if err := s.DeleteRack(ctx, rack.ID); err != store.ErrNotFound {
		t.Errorf("second delete: %v", err)
	}
}

func TestHostKeyMustBeConfirmedAgainWhenTheAddressChanges(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	h, err := s.SaveHost(ctx, store.Host{Name: "a52sf", Address: "10.0.0.9", Port: 22, Enabled: true})
	if err != nil || h.User != "hostinfo-ro" || h.HostKey != "" {
		t.Fatalf("%+v %v", h, err)
	}
	if err := s.TrustHostKey(ctx, h.ID, "ssh-ed25519 AAAAC3Nza"); err != nil {
		t.Fatal(err)
	}
	h, _ = s.Host(ctx, h.ID)
	h.Name = "renamed"
	h2, _ := s.SaveHost(ctx, h)
	if h2.HostKey == "" {
		t.Error("a rename must keep the confirmed key")
	}
	h2.Address = "10.9.9.9"
	h3, _ := s.SaveHost(ctx, h2)
	if h3.HostKey != "" {
		t.Error("another address must need a new confirmation")
	}
	for _, bad := range []store.Host{{Name: "x", Address: "a/b", Port: 22}, {Name: "x", Address: "u@h", Port: 22}, {Name: "x", Address: "h", Port: 0}, {Name: "x", Address: "h", Port: 22, User: "a b"}} {
		if _, err := s.SaveHost(ctx, bad); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

func TestSettingsDefaultsAndLimits(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	st, _ := s.Settings(ctx)
	if st != store.DefaultSettings() {
		t.Fatalf("%+v", st)
	}
	saved, _ := s.SaveSettings(ctx, store.Settings{PollSeconds: 1, HostPollSeconds: 90, WarnPercent: 120, RestartDelaySecond: 10, SampleRetentionDay: 30})
	if saved.PollSeconds != 10 || saved.WarnPercent != 80 || saved.HostPollSeconds != 90 || saved.RestartDelaySecond != 10 {
		t.Fatalf("out of range values must fall back: %+v", saved)
	}
	again, _ := s.Settings(ctx)
	if again != saved {
		t.Fatal("not stored")
	}
}

func TestAuditNewestFirstAndSamplesPrune(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	_ = s.Audit(ctx, "anna", "power.restart", "200U31", true)
	_ = s.Audit(ctx, "boris", "power.off", "200U32", false)
	log, _ := s.AuditLog(ctx, 10)
	if len(log) != 2 || log[0].User != "boris" || log[0].OK || !log[1].OK {
		t.Fatalf("%+v", log)
	}
	rack, _ := s.SaveRack(ctx, store.Rack{Name: "r", MaxAmps: 24})
	p, _ := s.SavePDU(ctx, store.PDU{RackID: rack.ID, Name: "A", Vendor: pdu.APC, Host: "h", Port: 161, MaxAmps: 24}, store.Communities{Read: "x"}, false)
	now := time.Now()
	amps := 5.5
	_ = s.AddSamples(ctx, []store.Sample{{PDUID: p.ID, Time: now.Add(-100 * 24 * time.Hour), Amps: &amps}, {PDUID: p.ID, Time: now.Add(-time.Hour), Amps: &amps}, {PDUID: p.ID, Time: now, Amps: &amps}})
	n, _ := s.PruneSamples(ctx, now.Add(-90*24*time.Hour))
	got, _ := s.Samples(ctx, p.ID, now.Add(-2*time.Hour))
	if n != 1 || len(got) != 2 || got[0].Amps == nil || *got[0].Amps != 5.5 {
		t.Fatalf("pruned %d, left %d", n, len(got))
	}
}

func TestLabels(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	rack, _ := s.SaveRack(ctx, store.Rack{Name: "r", MaxAmps: 24})
	_ = s.SetLabel(ctx, rack.ID, " 200U31 ", " web-01 ")
	l, _ := s.Labels(ctx)
	if l[rack.ID]["200u31"] != "web-01" {
		t.Fatalf("%v", l)
	}
	_ = s.SetLabel(ctx, rack.ID, "200u31", "")
	if l, _ = s.Labels(ctx); len(l[rack.ID]) != 0 {
		t.Fatal("not removed")
	}
}

func TestReopeningKeepsTheDataAndMigrationsRunOnce(t *testing.T) {
	s, path := open(t)
	ctx := context.Background()
	_, _ = s.SaveRack(ctx, store.Rack{Name: "r", MaxAmps: 24})
	s.Close()
	s2, err := store.Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if racks, _ := s2.Racks(ctx); len(racks) != 1 {
		t.Fatal("data lost")
	}
}

const macFile = `{
 "format": "pdu-monitor-settings", "version": 1, "exported": "2026-10-09T18:00:00Z",
 "racks": [{"id": "AAAA-1", "name": "200", "maxAmps": 24}, {"id": "AAAA-2", "name": "10F", "maxAmps": 20}],
 "devices": [
   {"id": "DDDD-1", "name": "P200A", "rackID": "AAAA-1", "vendor": "apc", "host": "10.0.0.5", "port": 161, "maxAmps": 24, "bankMaxAmps": 16},
   {"id": "DDDD-2", "name": "P200B", "rackID": "AAAA-1", "vendor": "cyberPower", "host": "10.0.0.6", "port": 161, "maxAmps": 24},
   {"id": "DDDD-3", "name": "Lost", "rackID": "ZZZZ", "vendor": "apc", "host": "10.0.0.7", "port": 161, "maxAmps": 24}],
 "labels": {"labels": {"AAAA-1": {"200u31": "web-01"}}},
 "settings": {"pollInterval": 15, "restartDelay": 9, "warnPercent": 85, "demoMode": false},
 "communities": {"DDDD-1": {"read": "pub", "write": "priv"}}
}`

func TestImportOfTheMacApplicationSettingsFile(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	sum, err := s.ImportMacBackup(ctx, []byte(macFile))
	if err != nil {
		t.Fatal(err)
	}
	if sum.Racks != 2 || sum.PDUs != 2 || sum.Labels != 1 || len(sum.WithoutCommunity) != 1 || sum.WithoutCommunity[0] != "P200B" || len(sum.Skipped) != 1 {
		t.Fatalf("%+v", sum)
	}
	pdus, _ := s.PDUs(ctx)
	byName := map[string]store.PDU{}
	for _, p := range pdus {
		byName[p.Name] = p
	}
	if byName["P200B"].Vendor != pdu.CyberPower || byName["P200A"].BankMaxAmps == nil || *byName["P200A"].BankMaxAmps != 16 {
		t.Fatalf("%+v", byName)
	}
	c, _ := s.PDUCommunities(ctx, byName["P200A"].ID)
	if c.Read != "pub" || c.Write != "priv" {
		t.Fatalf("%+v", c)
	}
	st, _ := s.Settings(ctx)
	if st.PollSeconds != 15 || st.RestartDelaySecond != 9 || st.WarnPercent != 85 {
		t.Fatalf("%+v", st)
	}
	// a second import changes nothing
	again, err := s.ImportMacBackup(ctx, []byte(macFile))
	if err != nil || again.Racks != 0 || again.PDUs != 0 {
		t.Fatalf("%+v %v", again, err)
	}
	if _, err := s.ImportMacBackup(ctx, []byte(`{"hello": 1}`)); err == nil {
		t.Fatal("a foreign file must be refused")
	}
	if _, err := s.ImportMacBackup(ctx, []byte(`not json`)); err == nil {
		t.Fatal("garbage must be refused")
	}
}

// A database made before collectors existed is upgraded and keeps its hosts as hosts that the server reads itself.
func TestUpgradeFromBeforeCollectors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "old.sqlite")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE hosts (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE, address TEXT NOT NULL, ssh_port INTEGER NOT NULL DEFAULT 22,
			ssh_user TEXT NOT NULL DEFAULT 'hostinfo-ro', rack_id INTEGER, outlet_key TEXT NOT NULL DEFAULT '', host_key TEXT NOT NULL DEFAULT '', enabled INTEGER NOT NULL DEFAULT 1)`,
		`CREATE TABLE schema_version (version INTEGER NOT NULL)`, `INSERT INTO schema_version VALUES (1)`,
		`INSERT INTO hosts (name, address, host_key) VALUES ('old1', '10.0.0.1', 'ssh-ed25519 AAAA')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	s, err := store.Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	hosts, err := s.Hosts(context.Background())
	if err != nil || len(hosts) != 1 || hosts[0].Name != "old1" || hosts[0].Collector != "" || hosts[0].HostKey == "" {
		t.Fatalf("%+v %v", hosts, err)
	}
}

func TestCollectedHosts(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	h, created, err := s.CollectedHost(ctx, "c-a", "x1")
	if err != nil || !created || h.Collector != "c-a" || !h.Enabled || h.Address != "" {
		t.Fatalf("%+v %v %v", h, created, err)
	}
	again, created, err := s.CollectedHost(ctx, "c-a", "x1")
	if err != nil || created || again.ID != h.ID {
		t.Fatalf("%+v %v %v", again, created, err)
	}
	if _, _, err := s.CollectedHost(ctx, "c-b", "x1"); !errors.Is(err, store.ErrOtherCollector) {
		t.Errorf("another collector: %v", err)
	}
	direct, _ := s.SaveHost(ctx, store.Host{Name: "d1", Address: "10.0.0.5", Port: 22, Enabled: true})
	if _, _, err := s.CollectedHost(ctx, "c-a", direct.Name); !errors.Is(err, store.ErrOtherCollector) {
		t.Errorf("a host that the server reads: %v", err)
	}
	if _, _, err := s.CollectedHost(ctx, "c-a", "  "); err == nil {
		t.Error("an empty name")
	}
	// editing a collected host keeps it a collected host, and a direct host still needs an address
	h.OutletKey = "100U1"
	if saved, err := s.SaveHost(ctx, h); err != nil || saved.Collector != "c-a" || saved.OutletKey != "100U1" {
		t.Errorf("%+v %v", saved, err)
	}
	if _, err := s.SaveHost(ctx, store.Host{Name: "d2", Port: 22}); err == nil {
		t.Error("a direct host without an address was saved")
	}
}

func TestServmanSettings(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	d := store.DefaultSettings()
	if d.ServmanPollMinutes != 60 || d.ServmanSyncMinutes != 0 || d.ServmanNameFormat != store.NameCCID {
		t.Fatalf("defaults: %+v", d)
	}
	saved, _ := s.SaveSettings(ctx, store.Settings{ServmanPollMinutes: 0, ServmanSyncMinutes: 30, ServmanNameFormat: store.NameCCIDName})
	if saved.ServmanPollMinutes != 0 || saved.ServmanSyncMinutes != 30 || saved.ServmanNameFormat != store.NameCCIDName {
		t.Fatalf("0 means off and must be kept: %+v", saved)
	}
	bad, _ := s.SaveSettings(ctx, store.Settings{ServmanPollMinutes: -5, ServmanSyncMinutes: 999999, ServmanNameFormat: "anything"})
	if bad.ServmanPollMinutes != 60 || bad.ServmanSyncMinutes != 0 || bad.ServmanNameFormat != store.NameCCID {
		t.Fatalf("out of range values fall back: %+v", bad)
	}
}
