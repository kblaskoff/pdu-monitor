package auth

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/kblaskoff/pdu-monitor/server/internal/store"
)

func sessionStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	key, _ := store.LoadOrCreateKey(filepath.Join(dir, "k"))
	sec, _ := store.NewSecrets(key)
	st, err := store.Open(filepath.Join(dir, "db"), sec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestSessionsSurviveARestart(t *testing.T) {
	st := sessionStore(t)
	now := time.Unix(1_800_000_000, 0)
	a := NewSessions(72*time.Hour, 72*time.Hour)
	a.Store, a.Now = st, func() time.Time { return now }
	anna := &User{Name: "Anna", Role: RoleAdmin, Source: "ldap"}
	token, sess := a.CreateWith(anna, "10.0.0.1", true, false)

	// a new process: the same store, nothing in memory
	b := NewSessions(72*time.Hour, 72*time.Hour)
	b.Store, b.Now = st, func() time.Time { return now.Add(24 * time.Hour) }
	if err := b.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := b.Get(token)
	if got == nil || got.User.Name != "Anna" || got.User.Role != RoleAdmin || got.CSRF != sess.CSRF || !got.MFA || got.IsLimited() {
		t.Fatalf("%+v", got)
	}
	if b.Get(token+"x") != nil || b.Get("") != nil {
		t.Error("another token")
	}
	// three days after the last request it is over, also after a restart
	c := NewSessions(72*time.Hour, 72*time.Hour)
	c.Store, c.Now = st, func() time.Time { return now.Add(24*time.Hour + 73*time.Hour) }
	_ = c.Load(context.Background())
	if c.Get(token) != nil {
		t.Error("a session older than the limit came back")
	}
	// logging out removes it for good
	d := NewSessions(72*time.Hour, 72*time.Hour)
	d.Store, d.Now = st, func() time.Time { return now }
	_ = d.Load(context.Background())
	d.Delete(token)
	e := NewSessions(72*time.Hour, 72*time.Hour)
	e.Store, e.Now = st, func() time.Time { return now }
	_ = e.Load(context.Background())
	if e.Get(token) != nil {
		t.Error("a session that was logged out came back")
	}
}

func TestLimitedSessionIsUpgradedAndUsersAreLoggedOutEverywhere(t *testing.T) {
	st := sessionStore(t)
	s := NewSessions(time.Hour, time.Hour)
	s.Store = st
	anna := &User{Name: "Anna", Source: "ldap", Role: RoleAdmin}
	t1, limited := s.CreateWith(anna, "ip", false, true)
	if !limited.IsLimited() || limited.MFA {
		t.Fatal("a session that may only set up the second step")
	}
	s.Upgrade(t1)
	if limited.IsLimited() || !limited.MFA {
		t.Error("not upgraded")
	}
	again := NewSessions(time.Hour, time.Hour)
	again.Store = st
	_ = again.Load(context.Background())
	if g := again.Get(t1); g == nil || g.IsLimited() || !g.MFA {
		t.Errorf("the upgrade must be kept: %+v", g)
	}
	t2, _ := s.Create(&User{Name: "anna", Source: "ldap"}, "ip") // the same person, another spelling of the name
	t3, _ := s.Create(&User{Name: "anna", Source: "local"}, "ip")
	t4, _ := s.Create(&User{Name: "Boris", Source: "ldap"}, "ip")
	s.DeleteUser(UserKey(anna))
	if s.Get(t1) != nil || s.Get(t2) != nil {
		t.Error("the sessions of the person remain")
	}
	if s.Get(t3) == nil || s.Get(t4) == nil {
		t.Error("the sessions of other people were removed")
	}
}

func TestLimitsCanBeChangedWhileRunning(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	s := NewSessions(time.Hour, time.Hour)
	s.Now = func() time.Time { return now }
	token, _ := s.Create(&User{Name: "a", Source: "ldap"}, "ip")
	now = now.Add(2 * time.Hour)
	s.SetLimits(72*time.Hour, 72*time.Hour) // the administrator made it three days before it ended
	if s.Get(token) == nil {
		t.Error("the new limit does not apply to an existing session")
	}
	now = now.Add(80 * time.Hour)
	if s.Get(token) != nil {
		t.Error("the limit")
	}
}
