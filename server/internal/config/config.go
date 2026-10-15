// Package config reads the configuration file (YAML).
package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/kblaskoff/pdu-monitor/server/internal/auth"
	"github.com/kblaskoff/pdu-monitor/server/internal/servman"
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
	// PublicURL is the address people type in the browser (https://pdu.example.com or https://pdu.example.com:8443). Passkeys need it: they
	// are tied to the name of the site, which must be a host name (not an IP address). Without it only authenticator apps are offered.
	PublicURL string `yaml:"public_url"`
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

	// Session is not used any more: how long people stay logged in is a setting of the web interface (Settings). The keys are still read so
	// that an old config.yaml loads.
	Session struct {
		IdleMinutes int `yaml:"idle_minutes"`
		MaxHours    int `yaml:"max_hours"`
	} `yaml:"session"`

	// Collectors are the processes (pdu-collector) that may push readings of hypervisors. The token itself is never stored here, only
	// its SHA-256 (make both with: pdumonitord collector-token -name NAME).
	Collectors []Collector `yaml:"collectors"`

	// Servman is the inventory that says which server is plugged into which outlet of which PDU: the outlets are named after it. The
	// intervals and the format of the names are in the settings of the web interface. Leave the section out to turn this off.
	Servman struct {
		URL string `yaml:"url"`
		// TokenFile holds the token of the API (one line, chmod 600). The token is never kept in this file or in the repository.
		TokenFile string `yaml:"token_file"`
		CAFile    string `yaml:"ca_file"`
		// InsecureSkipVerify turns the check of the server certificate off: only for an internal certificate whose CA you cannot give.
		InsecureSkipVerify bool     `yaml:"insecure_skip_verify"`
		Sources            []string `yaml:"sources"` // servers (the default) and netswitches
	} `yaml:"servman"`

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
	if c.PublicURL != "" {
		if _, _, err := c.RelyingParty(); err != nil {
			return err
		}
	}
	if c.Servman.URL != "" {
		if c.Servman.TokenFile == "" {
			return errors.New("servman.token_file: say where the token of the API is")
		}
		for _, src := range c.Servman.Sources {
			if src != "servers" && src != "netswitches" {
				return fmt.Errorf("servman.sources: %q is not known (servers, netswitches)", src)
			}
		}
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

// ServmanConfig is the connection to servman (the token is read from its file); ok is false when the section is not there.
func (c *Config) ServmanConfig() (cfg servman.Config, ok bool, err error) {
	if c.Servman.URL == "" {
		return cfg, false, nil
	}
	token, err := os.ReadFile(c.Servman.TokenFile)
	if err != nil {
		return cfg, true, err
	}
	return servman.Config{URL: c.Servman.URL, Token: strings.TrimSpace(string(token)), CAFile: c.Servman.CAFile,
		InsecureSkipVerify: c.Servman.InsecureSkipVerify, Sources: c.Servman.Sources}, true, nil
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

# servman says which server is plugged into which outlet; the outlets of the PDUs are named after it (200U13). How often it is asked
# and written, and whether the name has the machine name too, are in the settings of the web interface.
# servman:
#   url: "https://replace_me.example.com"
#   token_file: /etc/pdu-monitor/servman-token     # one line, chmod 640 root:pdumonitor
#   ca_file: /etc/pdu-monitor/servman-ca.pem       # or, if you cannot get the CA: insecure_skip_verify: true

# Collectors push the readings of the hypervisors (pdu-collector). Add one with: pdumonitord collector-token -name collector-a
# collectors:
#   - name: collector-a
#     token_sha256: "..."

# The address people type in the browser. It makes passkeys (a fingerprint on a laptop) possible; it must be a host name, not an IP address.
# public_url: https://pdu.example.com:8443

debug: false
`

// RelyingParty gives the host name and the origin that passkeys are tied to, from public_url.
func (c *Config) RelyingParty() (id, origin string, err error) {
	u, err := url.Parse(c.PublicURL)
	if err != nil || u.Host == "" || u.Path != "" && u.Path != "/" || u.RawQuery != "" || u.User != nil {
		return "", "", errors.New("public_url: write the address as https://name or https://name:port, nothing after it")
	}
	host := u.Hostname()
	if net.ParseIP(host) != nil {
		return "", "", errors.New("public_url: passkeys need a host name, not an IP address")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && host == "localhost") {
		return "", "", errors.New("public_url: passkeys need https (http works only for localhost)")
	}
	return host, u.Scheme + "://" + u.Host, nil
}
