// Package config reads the configuration file (YAML).
package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/kblaskoff/pdu-monitor/server/internal/auth"
)

// Config is the whole configuration file.
type Config struct {
	// Listen is the address of the web interface: the management address of this server, for example 10.0.0.5:8443.
	Listen string `yaml:"listen"`
	// TLSCert and TLSKey are the certificate (PEM) and its key. Without them the server refuses to start, unless allow_plain_http is set
	// (only for a lab or behind a reverse proxy that does the TLS).
	TLSCert        string `yaml:"tls_cert"`
	TLSKey         string `yaml:"tls_key"`
	AllowPlainHTTP bool   `yaml:"allow_plain_http"`
	// DataDir holds the database, the key that protects the communities and the SSH key of this server.
	DataDir string `yaml:"data_dir"`

	LDAP struct {
		URL                string              `yaml:"url"`
		StartTLS           bool                `yaml:"start_tls"`
		CAFile             string              `yaml:"ca_file"`
		InsecureSkipVerify bool                `yaml:"insecure_skip_verify"`
		LegacyTLS          bool                `yaml:"legacy_tls"`
		BindDN             string              `yaml:"bind_dn"`
		BindPasswordFile   string              `yaml:"bind_password_file"`
		UserBase           string              `yaml:"user_base"`
		UserFilter         string              `yaml:"user_filter"`
		UserDNTemplate     string              `yaml:"user_dn_template"`
		DisplayAttr        string              `yaml:"display_attr"`
		MemberOfAttr       string              `yaml:"member_of_attr"`
		GroupBase          string              `yaml:"group_base"`
		GroupFilter        string              `yaml:"group_filter"`
		TimeoutSeconds     int                 `yaml:"timeout_seconds"`
		Roles              map[string][]string `yaml:"roles"`
	} `yaml:"ldap"`

	// LocalAdmin is an emergency account for the day the directory is down. Off unless a bcrypt hash is given.
	LocalAdmin struct {
		Username     string `yaml:"username"`
		PasswordHash string `yaml:"password_hash"`
	} `yaml:"local_admin"`

	Session struct {
		IdleMinutes int `yaml:"idle_minutes"`
		MaxHours    int `yaml:"max_hours"`
	} `yaml:"session"`

	// Collectors are the processes (pdu-collector) that may push readings of hypervisors. The token itself is never stored here, only
	// its SHA-256 (make both with: pdumonitord collector-token -name NAME).
	Collectors []Collector `yaml:"collectors"`

	// Debug writes every SNMP request and every step of the login (never a password) to the log.
	Debug bool `yaml:"debug"`
}

// Collector is one allowed collector.
type Collector struct {
	Name      string `yaml:"name"`
	TokenHash string `yaml:"token_sha256"`
}

// Load reads and checks a file. Unknown keys are an error: a typo must not silently turn a protection off.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := c.finish(filepath.Dir(path)); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

func (c *Config) finish(dir string) error {
	if c.Listen == "" {
		c.Listen = "127.0.0.1:8443"
	}
	if c.DataDir == "" {
		c.DataDir = "/var/lib/pdu-monitor"
	}
	if (c.TLSCert == "") != (c.TLSKey == "") {
		return errors.New("tls_cert and tls_key must be given together")
	}
	if c.TLSCert == "" && !c.AllowPlainHTTP {
		return errors.New("no TLS certificate: set tls_cert and tls_key (or allow_plain_http: true for a lab)")
	}
	if c.Session.IdleMinutes <= 0 {
		c.Session.IdleMinutes = 480
	}
	if c.Session.MaxHours <= 0 {
		c.Session.MaxHours = 24
	}
	seen := map[string]bool{}
	for i, col := range c.Collectors {
		c.Collectors[i].Name = strings.TrimSpace(col.Name)
		c.Collectors[i].TokenHash = strings.ToLower(strings.TrimSpace(col.TokenHash))
		col = c.Collectors[i]
		if col.Name == "" || len(col.Name) > 60 || strings.ContainsAny(col.Name, " \t\r\n") {
			return fmt.Errorf("collectors[%d]: give the collector a short name without spaces", i)
		}
		if seen[col.Name] {
			return fmt.Errorf("collectors: the name %q is used twice", col.Name)
		}
		seen[col.Name] = true
		if b, err := hex.DecodeString(col.TokenHash); err != nil || len(b) != sha256.Size {
			return fmt.Errorf("collectors[%d] (%s): token_sha256 must be 64 hex characters (pdumonitord collector-token makes it)", i, col.Name)
		}
	}
	return nil
}

// LDAPConfig converts the section for the auth package; the bind password is read from its own file (mode 0600).
func (c *Config) LDAPConfig() (auth.LDAPConfig, error) {
	l := c.LDAP
	cfg := auth.LDAPConfig{URL: l.URL, StartTLS: l.StartTLS, CAFile: l.CAFile, InsecureSkipVerify: l.InsecureSkipVerify, LegacyTLS: l.LegacyTLS,
		BindDN: l.BindDN, UserBase: l.UserBase, UserFilter: l.UserFilter, UserDNTemplate: l.UserDNTemplate, DisplayAttr: l.DisplayAttr,
		MemberOfAttr: l.MemberOfAttr, GroupBase: l.GroupBase, GroupFilter: l.GroupFilter, Timeout: time.Duration(l.TimeoutSeconds) * time.Second,
		Roles: map[auth.Role][]string{}}
	for name, groups := range l.Roles {
		role := auth.Role(strings.ToLower(name))
		if role.Rank() == 0 {
			return cfg, fmt.Errorf("ldap.roles: %q is not a role (admin, operator or viewer)", name)
		}
		cfg.Roles[role] = groups
	}
	if l.BindPasswordFile != "" {
		data, err := os.ReadFile(l.BindPasswordFile)
		if err != nil {
			return cfg, err
		}
		cfg.BindPassword = strings.TrimRight(string(data), "\r\n")
	}
	return cfg, nil
}

// Example is written by `pdumonitord init`.
const Example = `# PDU Monitor server. Replace every replace_me with the values of your network.
# Readable by root and the group pdumonitor only: chown root:pdumonitor, chmod 640.

# ":8443" listens on every address of this server, "10.0.0.5:8443" on one. An IP address is safer than a name here
# (a name is looked up when the service starts); the name people type only has to be in the DNS and in the certificate.
listen: ":8443"
# PEM files: tls_cert is the certificate of the server followed by the intermediate certificates (the full chain),
# tls_key is its private key without a passphrase. Both: chown root:pdumonitor, chmod 640.
tls_cert: /etc/pdu-monitor/tls.crt
tls_key: /etc/pdu-monitor/tls.key

data_dir: /var/lib/pdu-monitor

# The directory is used for one thing only: the login to the web interface.
ldap:
  url: "ldaps://ldap.replace_me:636"       # or ldap://host:389 with start_tls: true
  # start_tls: true
  # ca_file: /etc/pdu-monitor/ldap-ca.pem   # only if the certificate of the directory is from your own CA; the file must exist
  # legacy_tls: true                        # only if the directory is too old for TLS 1.2
  bind_dn: "cn=replace_me,dc=replace_me,dc=replace_me"   # a service account that may search (leave out for anonymous search)
  bind_password_file: /etc/pdu-monitor/ldap-bind-password # one line; chown root:pdumonitor, chmod 640
  user_base: "ou=People,dc=replace_me,dc=replace_me"
  user_filter: "(uid={username})"           # Active Directory: (&(objectClass=user)(sAMAccountName={username}))
  # user_dn_template: "uid={username},ou=People,dc=replace_me,dc=replace_me"   # instead of the search, if the directory does not allow it
  group_base: "ou=group,dc=replace_me,dc=replace_me"   # leave out if only memberOf is used
  # group_filter: "(memberUid={username})"  # the default also finds groups by member / uniqueMember (the DN of the person)
  # member_of_attr: memberOf                # the attribute of the person that lists the groups (Active Directory, OpenLDAP memberof overlay)
  roles:
    admin: [admins]                         # the cn (or the full DN) of the groups; nobody outside them can log in

# Emergency account, for the day the directory is down. Make the hash with: pdumonitord hash-password
# local_admin:
#   username: emergency
#   password_hash: "$2a$12$..."

# Collectors push the readings of the hypervisors (pdu-collector). Add one with: pdumonitord collector-token -name collector-a
# collectors:
#   - name: collector-a
#     token_sha256: "..."

session:
  idle_minutes: 480
  max_hours: 24

debug: false
`
