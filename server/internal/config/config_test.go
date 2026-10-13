package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kblaskoff/pdu-monitor/server/internal/auth"
)

func write(t *testing.T, text string) string {
	p := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestTheExampleIsAValidFile(t *testing.T) {
	c, err := Load(write(t, Example))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":8443" || c.Session.IdleMinutes != 480 || c.LDAP.Roles["admin"][0] != "admins" {
		t.Fatalf("%+v", c)
	}
	dir := t.TempDir()
	pw := filepath.Join(dir, "pw")
	os.WriteFile(pw, []byte("secret\n"), 0o600)
	c.LDAP.BindPasswordFile = pw
	l, err := c.LDAPConfig()
	if err != nil || l.BindPassword != "secret" || l.Roles[auth.RoleAdmin][0] != "admins" || l.URL != "ldaps://ldap.replace_me:636" {
		t.Fatalf("%+v %v", l, err)
	}
}

func TestMistakesAreErrorsNotSilentDefaults(t *testing.T) {
	cases := map[string]string{
		"unknown key (a typo)":  "listen: ':1'\ntls_cert: a\ntls_key: b\nldapp: {}\n",
		"cert without key":      "tls_cert: a\n",
		"no tls and no opt-out": "listen: ':1'\n",
		"not yaml":              "listen: [\n",
	}
	for name, text := range cases {
		if _, err := Load(write(t, text)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("missing file")
	}
	c, err := Load(write(t, "allow_plain_http: true\n"))
	if err != nil || c.Listen != "127.0.0.1:8443" || c.DataDir != "/var/lib/pdu-monitor" {
		t.Fatalf("defaults: %+v %v", c, err)
	}
	c.LDAP.Roles = map[string][]string{"emperor": {"x"}}
	if _, err := c.LDAPConfig(); err == nil || !strings.Contains(err.Error(), "emperor") {
		t.Fatalf("%v", err)
	}
}

const goodHash = "7be78af209b4ef4e54e3a3709a4224b361dc2ea5b63115d97b8496e0688bee38"

func TestCollectors(t *testing.T) {
	c, err := Load(write(t, "allow_plain_http: true\ncollectors:\n  - name: collector-a\n    token_sha256: \""+strings.ToUpper(goodHash)+"\"\n  - name: collector-b\n    token_sha256: "+goodHash+"\n"))
	if err != nil || len(c.Collectors) != 2 || c.Collectors[0].TokenHash != goodHash {
		t.Fatalf("%+v %v", c, err)
	}
	bad := map[string]string{
		"no name":      "collectors:\n  - token_sha256: " + goodHash + "\n",
		"space":        "collectors:\n  - name: a b\n    token_sha256: " + goodHash + "\n",
		"twice":        "collectors:\n  - name: a\n    token_sha256: " + goodHash + "\n  - name: a\n    token_sha256: " + goodHash + "\n",
		"short hash":   "collectors:\n  - name: a\n    token_sha256: abcd\n",
		"not hex":      "collectors:\n  - name: a\n    token_sha256: " + strings.Repeat("z", 64) + "\n",
		"token itself": "collectors:\n  - name: a\n    token: abc\n",
	}
	for name, text := range bad {
		if _, err := Load(write(t, "allow_plain_http: true\n"+text)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
