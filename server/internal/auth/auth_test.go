package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

func newDir(t *testing.T, cfg LDAPConfig) *Directory {
	t.Helper()
	d, err := NewDirectory(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestOpenLDAPStyleDirectoryWithGroupSearch(t *testing.T) {
	fake := openLDAPDirectory()
	startDirectory(t, fake, nil)
	d := newDir(t, openLDAPConfig(fake, "ldap"))
	ctx := context.Background()

	u, err := d.Authenticate(ctx, "anna", "anna-pw")
	if err != nil || u.Role != RoleAdmin || u.DisplayName != "Anna Petrova" || u.Name != "anna" || u.Source != "ldap" {
		t.Fatalf("%+v %v", u, err)
	}
	// memberUid of a posixGroup gives the operator role
	b, err := d.Authenticate(ctx, "boris", "boris-pw")
	if err != nil || b.Role != RoleOperator {
		t.Fatalf("%+v %v", b, err)
	}
	// clara is in "staff" only: right password, but no role
	if _, err := d.Authenticate(ctx, "clara", "clara-pw"); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("%v", err)
	}
	// wrong password and unknown user look the same
	if _, err := d.Authenticate(ctx, "anna", "nope"); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("%v", err)
	}
	if _, err := d.Authenticate(ctx, "nobody", "x"); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("%v", err)
	}
	// two entries with the same uid: refused, not guessed
	if _, err := d.Authenticate(ctx, "twin", "t1"); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("%v", err)
	}
}

func TestAnEmptyPasswordNeverReachesTheDirectory(t *testing.T) {
	fake := openLDAPDirectory()
	startDirectory(t, fake, nil)
	d := newDir(t, openLDAPConfig(fake, "ldap"))
	if _, err := d.Authenticate(context.Background(), "anna", ""); !errors.Is(err, ErrBadCredentials) {
		t.Fatal(err)
	}
	for _, b := range fake.binds {
		if strings.Contains(b, "anna") {
			t.Fatalf("a bind for anna was sent without a password: %v", fake.binds)
		}
	}
}

func TestTheUserNameCannotChangeTheFilter(t *testing.T) {
	fake := openLDAPDirectory()
	startDirectory(t, fake, nil)
	d := newDir(t, openLDAPConfig(fake, "ldap"))
	for _, evil := range []string{"*", "anna)(uid=*", "*)(|(uid=*", "anna)(|(cn=*", `a\`} {
		if _, err := d.Authenticate(context.Background(), evil, "anna-pw"); !errors.Is(err, ErrBadCredentials) {
			t.Errorf("%q: %v", evil, err)
		}
	}
	for _, f := range fake.filters {
		if strings.Count(f, "(") != strings.Count(f, ")") || strings.Contains(f, "uid=*") || strings.Contains(f, "(|(cn=") && strings.Contains(f, "anna)") {
			t.Errorf("an unescaped filter reached the directory: %s", f)
		}
	}
}

func TestADStyleMemberOfWithoutAGroupSearch(t *testing.T) {
	fake := &fakeDirectory{serviceDN: "cn=svc,dc=corp,dc=example", servicePW: "pw", entries: []dirEntry{
		{dn: "cn=Dimitar Georgiev,ou=users,dc=corp,dc=example", password: "d-pw", attrs: map[string][]string{
			"sAMAccountName": {"dimitar"}, "objectClass": {"user"}, "cn": {"Dimitar Georgiev"},
			"memberOf": {"CN=Domain Users,CN=Users,DC=corp,DC=example", "CN=Admins,OU=Groups,DC=corp,DC=example"}}},
	}}
	startDirectory(t, fake, nil)
	d := newDir(t, LDAPConfig{URL: "ldap://" + fake.addr, BindDN: fake.serviceDN, BindPassword: "pw", UserBase: "ou=users,dc=corp,dc=example",
		UserFilter: "(&(objectClass=user)(sAMAccountName={username}))", Roles: map[Role][]string{RoleAdmin: {"admins"}}, Timeout: 3 * time.Second})
	u, err := d.Authenticate(context.Background(), "Dimitar", "d-pw")
	if err != nil || u.Role != RoleAdmin {
		t.Fatalf("%+v %v", u, err)
	}
}

func TestDirectWithoutAServiceAccountAndWithADNTemplate(t *testing.T) {
	fake := openLDAPDirectory()
	// nothing may be searched anonymously; the person's own bind is what reads the groups
	startDirectory(t, fake, nil)
	cfg := openLDAPConfig(fake, "ldap")
	cfg.BindDN, cfg.BindPassword = "", ""
	cfg.UserBase, cfg.UserFilter = "", ""
	cfg.UserDNTemplate = "uid={username},ou=people,dc=example,dc=com"
	d := newDir(t, cfg)
	u, err := d.Authenticate(context.Background(), "anna", "anna-pw")
	if err != nil || u.Role != RoleAdmin {
		t.Fatalf("%+v %v", u, err)
	}
	if _, err := d.Authenticate(context.Background(), "anna", "wrong"); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("%v", err)
	}
	// the name is escaped as a DN too: it cannot reach into another branch of the tree
	if _, err := d.Authenticate(context.Background(), "x,ou=people,dc=example,dc=com", "anna-pw"); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("%v", err)
	}
}

func TestWrongServiceAccountOrNoServerIsUnavailableNotBadCredentials(t *testing.T) {
	fake := openLDAPDirectory()
	startDirectory(t, fake, nil)
	cfg := openLDAPConfig(fake, "ldap")
	cfg.BindPassword = "wrong"
	if _, err := newDir(t, cfg).Authenticate(context.Background(), "anna", "anna-pw"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("%v", err)
	}
	cfg = openLDAPConfig(fake, "ldap")
	cfg.URL = "ldap://" + freeAddr(t)
	if _, err := newDir(t, cfg).Authenticate(context.Background(), "anna", "anna-pw"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("%v", err)
	}
}

func TestLDAPSWithAnInternalCA(t *testing.T) {
	fake := openLDAPDirectory()
	conf, ca := selfSigned(t, "ldap.example.com")
	startDirectory(t, fake, conf)
	cfg := openLDAPConfig(fake, "ldaps")
	cfg.URL = "ldaps://" + fake.addr
	// the certificate is for ldap.example.com, the test connects to 127.0.0.1: the IP is in the certificate too
	cfg.CAFile = ca
	if u, err := newDir(t, cfg).Authenticate(context.Background(), "anna", "anna-pw"); err != nil || u.Role != RoleAdmin {
		t.Fatalf("with the CA: %+v %v", u, err)
	}
	cfg.CAFile = ""
	if _, err := newDir(t, cfg).Authenticate(context.Background(), "anna", "anna-pw"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("an unknown CA must be refused: %v", err)
	}
	cfg.InsecureSkipVerify = true
	if u, err := newDir(t, cfg).Authenticate(context.Background(), "anna", "anna-pw"); err != nil || u == nil {
		t.Fatalf("skip verify: %v", err)
	}
}

func TestStartTLSOnThePlainPort(t *testing.T) {
	fake := openLDAPDirectory()
	conf, ca := selfSigned(t, "ldap.example.com")
	fake.startTLS = conf
	startDirectory(t, fake, nil)
	cfg := openLDAPConfig(fake, "ldap")
	cfg.StartTLS, cfg.CAFile = true, ca
	if u, err := newDir(t, cfg).Authenticate(context.Background(), "anna", "anna-pw"); err != nil || u.Role != RoleAdmin {
		t.Fatalf("%+v %v", u, err)
	}
	cfg.CAFile = ""
	if _, err := newDir(t, cfg).Authenticate(context.Background(), "anna", "anna-pw"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("an unknown CA must be refused: %v", err)
	}
}

func TestRoleOfTakesTheHighestAndMatchesByCNOrDN(t *testing.T) {
	roles := map[Role][]string{RoleAdmin: {"Admins"}, RoleOperator: {"cn=ops,ou=groups,dc=example,dc=com"}, RoleViewer: {"everyone"}}
	got, matched := roleOf(roles, map[string]bool{"cn=ops,ou=groups,dc=example,dc=com": true, "cn=everyone,ou=g,dc=x": true})
	if got != RoleOperator || len(matched) != 2 {
		t.Fatalf("%s %v", got, matched)
	}
	got, _ = roleOf(roles, map[string]bool{"CN=ADMINS,OU=Groups,DC=corp": true, "cn=everyone,ou=g,dc=x": true})
	if got != RoleAdmin {
		t.Fatal(got)
	}
	if got, _ := roleOf(roles, map[string]bool{"cn=other,ou=g,dc=x": true}); got != "" {
		t.Fatal(got)
	}
	if got, _ := roleOf(map[Role][]string{RoleAdmin: {"admins"}}, map[string]bool{"cn=admins-extra,dc=x": true}); got != "" {
		t.Fatal("a longer name is not the same group")
	}
}

func TestConfigValidation(t *testing.T) {
	good := LDAPConfig{URL: "ldaps://ldap.example.com", UserBase: "dc=x", Roles: map[Role][]string{RoleAdmin: {"admins"}}}
	if _, err := NewDirectory(good); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*LDAPConfig){
		"bad url":      func(c *LDAPConfig) { c.URL = "http://x" },
		"no base":      func(c *LDAPConfig) { c.UserBase = "" },
		"no roles":     func(c *LDAPConfig) { c.Roles = nil },
		"filter":       func(c *LDAPConfig) { c.UserFilter = "(uid=fixed)" },
		"template":     func(c *LDAPConfig) { c.UserBase = ""; c.UserDNTemplate = "uid=fixed,dc=x" },
		"empty url":    func(c *LDAPConfig) { c.URL = "" },
		"empty groups": func(c *LDAPConfig) { c.Roles = map[Role][]string{RoleAdmin: {}} },
	} {
		c := good
		mutate(&c)
		if _, err := NewDirectory(c); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestLocalEmergencyAccount(t *testing.T) {
	hash, _ := bcrypt.GenerateFromPassword([]byte("break-glass"), bcrypt.MinCost)
	chain := &Chain{Local: &LocalAdmin{Username: "emergency", Hash: string(hash)}}
	ctx := context.Background()
	u, err := chain.Authenticate(ctx, "Emergency", "break-glass")
	if err != nil || u.Role != RoleAdmin || u.Source != "local" {
		t.Fatalf("%+v %v", u, err)
	}
	if _, err := chain.Authenticate(ctx, "emergency", "wrong"); !errors.Is(err, ErrBadCredentials) {
		t.Fatal(err)
	}
	// another name goes to the directory, and there is none here
	if _, err := chain.Authenticate(ctx, "anna", "break-glass"); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	// without a hash the account is off
	off := &Chain{Local: &LocalAdmin{Username: "emergency"}}
	if _, err := off.Authenticate(ctx, "emergency", ""); !errors.Is(err, ErrBadCredentials) {
		t.Fatal(err)
	}
	if _, err := off.Authenticate(ctx, "emergency", "anything"); !errors.Is(err, ErrBadCredentials) {
		t.Fatal(err)
	}
	for _, bad := range []string{"", " x", "a\nb", strings.Repeat("a", 200)} {
		if _, err := chain.Authenticate(ctx, bad, "pw"); !errors.Is(err, ErrBadCredentials) {
			t.Errorf("%q: %v", bad, err)
		}
	}
}

func TestSessions(t *testing.T) {
	now := time.Unix(1_760_000_000, 0)
	s := NewSessions(30*time.Minute, 8*time.Hour)
	s.Now = func() time.Time { return now }
	token, sess := s.Create(&User{Name: "anna", Role: RoleAdmin}, "10.0.0.1")
	if s.Get(token) == nil || s.Get("") != nil || s.Get("nonsense") != nil {
		t.Fatal("lookup")
	}
	if !sess.CheckCSRF(sess.CSRF) || sess.CheckCSRF("") || sess.CheckCSRF(sess.CSRF+"x") {
		t.Fatal("csrf")
	}
	now = now.Add(20 * time.Minute)
	if s.Get(token) == nil {
		t.Fatal("still active")
	}
	now = now.Add(20 * time.Minute) // 20 minutes since the last request: still fine
	if s.Get(token) == nil {
		t.Fatal("activity must extend the session")
	}
	now = now.Add(31 * time.Minute)
	if s.Get(token) != nil {
		t.Fatal("idle timeout")
	}
	token, _ = s.Create(&User{Name: "boris"}, "")
	for i := 0; i < 20; i++ { // always active, but 8 hours is the limit
		now = now.Add(25 * time.Minute)
		if s.Get(token) == nil {
			break
		}
	}
	if s.Get(token) != nil {
		t.Fatal("absolute limit")
	}
	token, _ = s.Create(&User{Name: "c"}, "")
	s.Delete(token)
	if s.Get(token) != nil {
		t.Fatal("logout")
	}
	s.Reap()
}

func TestLimiter(t *testing.T) {
	now := time.Unix(1_760_000_000, 0)
	l := NewLimiter(3, 10*time.Minute)
	l.Now = func() time.Time { return now }
	for i := 0; i < 3; i++ {
		if ok, _ := l.Allow("ip:1"); !ok {
			t.Fatal("allowed so far")
		}
		l.Fail("ip:1")
	}
	ok, retry := l.Allow("ip:1")
	if ok || retry <= 0 || retry > 10*time.Minute {
		t.Fatalf("%v %v", ok, retry)
	}
	if ok, _ := l.Allow("ip:2"); !ok {
		t.Fatal("another key is not affected")
	}
	now = now.Add(11 * time.Minute)
	if ok, _ := l.Allow("ip:1"); !ok {
		t.Fatal("the window has passed")
	}
	l.Fail("ip:3")
	l.Reset("ip:3")
	l.Fail("ip:3")
	l.Fail("ip:3")
	if ok, _ := l.Allow("ip:3"); !ok {
		t.Fatal("a success must clear the failures")
	}
}

func TestRoleOrdering(t *testing.T) {
	if !RoleAdmin.Can(RoleOperator) || !RoleOperator.Can(RoleViewer) || RoleViewer.Can(RoleOperator) || Role("").Can(RoleViewer) || RoleAdmin.Can("") {
		t.Fatal("roles")
	}
}
