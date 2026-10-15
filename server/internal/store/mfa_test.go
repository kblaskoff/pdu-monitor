package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func mfaStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	key, _ := LoadOrCreateKey(filepath.Join(dir, "k"))
	sec, _ := NewSecrets(key)
	st, err := Open(filepath.Join(dir, "db"), sec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestFactorsAreKeptPerPersonAndTheSecretIsEncrypted(t *testing.T) {
	st := mfaStore(t)
	ctx := context.Background()
	id, err := st.AddTOTP(ctx, "ldap:anna", "phone", []byte("12345678901234567890"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddPasskey(ctx, "ldap:anna", "laptop", []byte("cred-1"), []byte("cose"), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddTOTP(ctx, "ldap:boris", "his phone", []byte("secret-of-boris-0000")); err != nil {
		t.Fatal(err)
	}
	fs, _ := st.Factors(ctx, "ldap:anna")
	if len(fs) != 2 || fs[0].Kind != FactorTOTP || string(fs[0].Secret) != "12345678901234567890" || fs[1].Kind != FactorPasskey || string(fs[1].CredID) != "cred-1" {
		t.Fatalf("%+v", fs)
	}
	var raw []byte
	if err := st.db.QueryRow(`SELECT secret_enc FROM auth_factors WHERE id = ?`, id).Scan(&raw); err != nil || len(raw) == 0 || string(raw) == "12345678901234567890" {
		t.Errorf("the secret must be encrypted in the file: %q %v", raw, err)
	}
	if other, _ := st.Factors(ctx, "ldap:boris"); len(other) != 1 || other[0].Name != "his phone" {
		t.Errorf("%+v", other)
	}
	if ok, _ := st.RemoveFactor(ctx, "ldap:boris", id); ok {
		t.Error("a person removed the device of another")
	}
	if ok, _ := st.RemoveFactor(ctx, "ldap:anna", id); !ok {
		t.Error("the owner could not remove it")
	}
	users, _ := st.UsersWithFactors(ctx)
	if len(users) != 2 || users[0].User != "ldap:anna" || users[0].TOTP != 0 || users[0].Passkeys != 1 {
		t.Errorf("%+v", users)
	}
}

func TestAtMostThreeOfEachKind(t *testing.T) {
	st := mfaStore(t)
	ctx := context.Background()
	for i := 0; i < MaxFactorsPerKind; i++ {
		if _, err := st.AddTOTP(ctx, "ldap:anna", "phone", []byte("12345678901234567890")); err != nil {
			t.Fatal(err)
		}
		if _, err := st.AddPasskey(ctx, "ldap:anna", "key", []byte{byte(i)}, []byte("k"), 0); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.AddTOTP(ctx, "ldap:anna", "a fourth", []byte("12345678901234567890")); !errors.Is(err, ErrTooManyFactors) {
		t.Errorf("%v", err)
	}
	if _, err := st.AddPasskey(ctx, "ldap:anna", "a fourth", []byte{9}, []byte("k"), 0); !errors.Is(err, ErrTooManyFactors) {
		t.Errorf("%v", err)
	}
	if _, err := st.AddTOTP(ctx, "ldap:boris", "his", []byte("12345678901234567890")); err != nil {
		t.Errorf("the limit is per person: %v", err)
	}
	if _, err := st.AddPasskey(ctx, "ldap:boris", "same key", []byte{1}, []byte("k"), 0); err == nil {
		t.Error("the same credential id for two devices")
	}
}

func TestACodeAndACounterCannotBeUsedTwice(t *testing.T) {
	st := mfaStore(t)
	ctx := context.Background()
	id, _ := st.AddTOTP(ctx, "ldap:anna", "phone", []byte("12345678901234567890"))
	if ok, _ := st.UseTOTPStep(ctx, id, 100); !ok {
		t.Fatal("the first use")
	}
	for _, step := range []int64{100, 99, 1} {
		if ok, _ := st.UseTOTPStep(ctx, id, step); ok {
			t.Errorf("step %d accepted after 100", step)
		}
	}
	if ok, _ := st.UseTOTPStep(ctx, id, 101); !ok {
		t.Error("a later step")
	}
	pk, _ := st.AddPasskey(ctx, "ldap:anna", "key", []byte("c"), []byte("k"), 5)
	if ok, _ := st.UsePasskeyCount(ctx, pk, 5); ok {
		t.Error("the same counter")
	}
	if ok, _ := st.UsePasskeyCount(ctx, pk, 4); ok {
		t.Error("a counter that went back")
	}
	if ok, _ := st.UsePasskeyCount(ctx, pk, 6); !ok {
		t.Error("a counter that went up")
	}
	zero, _ := st.AddPasskey(ctx, "ldap:anna", "synced", []byte("z"), []byte("k"), 0)
	for i := 0; i < 3; i++ {
		if ok, _ := st.UsePasskeyCount(ctx, zero, 0); !ok {
			t.Error("a passkey that never counts must work every time")
		}
	}
}

func TestRecoveryCodesWorkOnce(t *testing.T) {
	st := mfaStore(t)
	ctx := context.Background()
	if err := st.ReplaceRecoveryCodes(ctx, "ldap:anna", []string{"h1", "h2", "h3"}); err != nil {
		t.Fatal(err)
	}
	if n, _ := st.RecoveryLeft(ctx, "ldap:anna"); n != 3 {
		t.Fatal(n)
	}
	if ok, _ := st.UseRecoveryCode(ctx, "ldap:anna", "h2"); !ok {
		t.Error("a code")
	}
	if ok, _ := st.UseRecoveryCode(ctx, "ldap:anna", "h2"); ok {
		t.Error("the same code twice")
	}
	if ok, _ := st.UseRecoveryCode(ctx, "ldap:boris", "h1"); ok {
		t.Error("the code of another person")
	}
	if err := st.ReplaceRecoveryCodes(ctx, "ldap:anna", []string{"n1"}); err != nil {
		t.Fatal(err)
	}
	if ok, _ := st.UseRecoveryCode(ctx, "ldap:anna", "h1"); ok {
		t.Error("an old code after new ones were made")
	}
	st.AddTOTP(ctx, "ldap:anna", "phone", []byte("12345678901234567890"))
	if n, err := st.ResetFactors(ctx, "ldap:anna"); err != nil || n != 1 {
		t.Errorf("%d %v", n, err)
	}
	if n, _ := st.RecoveryLeft(ctx, "ldap:anna"); n != 0 {
		t.Error("the codes survive a reset")
	}
}

func TestSessionsAreKeptAndOldOnesAreDropped(t *testing.T) {
	st := mfaStore(t)
	ctx := context.Background()
	now := time.Now()
	for k, seen := range map[string]time.Time{"fresh": now, "old": now.Add(-100 * time.Hour)} {
		if err := st.SaveSession(ctx, StoredSession{Key: k, UserJSON: `{"name":"anna"}`, CSRF: "c-" + k, Created: seen, LastSeen: seen, IP: "10.0.0.1", MFA: true}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.LoadSessions(ctx, now.Add(-72*time.Hour))
	if err != nil || len(got) != 1 || got[0].Key != "fresh" || got[0].CSRF != "c-fresh" || !got[0].MFA {
		t.Fatalf("%+v %v", got, err)
	}
	_ = st.TouchSession(ctx, "fresh", now.Add(time.Hour))
	_ = st.DeleteSession(ctx, "fresh")
	if got, _ := st.LoadSessions(ctx, now.Add(-72*time.Hour)); len(got) != 0 {
		t.Errorf("%+v", got)
	}
}
