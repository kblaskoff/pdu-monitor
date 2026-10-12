package auth

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	ldap "github.com/go-ldap/ldap/v3"
)

// LDAPConfig says how to find a person and their groups in the directory. The defaults suit OpenLDAP; for Active Directory use
// user_filter (&(objectClass=user)(sAMAccountName={username})) and the memberOf attribute.
type LDAPConfig struct {
	URL      string // ldaps://host:636, or ldap://host:389 with StartTLS
	StartTLS bool
	// CAFile is a PEM file with the certificate of the authority that signed the directory's certificate (an internal CA).
	CAFile             string
	InsecureSkipVerify bool
	// LegacyTLS allows TLS 1.0 and the old cipher suites, for a directory server that is too old for the current defaults.
	LegacyTLS bool

	// BindDN and BindPassword are a service account that searches for people and groups. Without them the search is anonymous.
	BindDN       string
	BindPassword string

	// The person is found by UserFilter under UserBase ({username} is replaced, escaped), or built from UserDNTemplate
	// ("uid={username},ou=people,dc=example,dc=com") when the directory does not allow searching.
	UserBase       string
	UserFilter     string
	UserDNTemplate string
	DisplayAttr    string

	// Groups of the person: the attribute of the person (memberOf), and/or a search under GroupBase with GroupFilter
	// ({dn} and {username} are replaced, escaped).
	MemberOfAttr string
	GroupBase    string
	GroupFilter  string

	// Roles maps a role to the groups (their cn or their full DN) that give it. A person with none of them cannot log in.
	Roles map[Role][]string

	Timeout time.Duration
	// Debug, when set, gets what happens step by step (never the password).
	Debug func(string)
}

func (c *LDAPConfig) defaults() {
	if c.UserFilter == "" && c.UserDNTemplate == "" {
		c.UserFilter = "(uid={username})"
	}
	if c.DisplayAttr == "" {
		c.DisplayAttr = "cn"
	}
	if c.GroupBase != "" && c.GroupFilter == "" {
		c.GroupFilter = "(|(member={dn})(uniqueMember={dn})(memberUid={username}))"
	}
	if c.MemberOfAttr == "" {
		c.MemberOfAttr = "memberOf"
	}
	if c.Timeout <= 0 {
		c.Timeout = 10 * time.Second
	}
}

// Validate checks the configuration when the program starts.
func (c *LDAPConfig) Validate() error {
	c.defaults()
	u, err := url.Parse(c.URL)
	if err != nil || (u.Scheme != "ldap" && u.Scheme != "ldaps") || u.Host == "" {
		return errors.New("ldap.url must look like ldaps://host:636 or ldap://host:389")
	}
	if c.UserBase == "" && c.UserDNTemplate == "" {
		return errors.New("ldap.user_base (or ldap.user_dn_template) is needed")
	}
	if c.UserDNTemplate != "" && !strings.Contains(c.UserDNTemplate, "{username}") {
		return errors.New("ldap.user_dn_template must contain {username}")
	}
	if c.UserFilter != "" && !strings.Contains(c.UserFilter, "{username}") {
		return errors.New("ldap.user_filter must contain {username}")
	}
	n := 0
	for _, groups := range c.Roles {
		n += len(groups)
	}
	if n == 0 {
		return errors.New("ldap.roles: at least one group must give a role (for example admin: [admins])")
	}
	if u.Scheme == "ldap" && !c.StartTLS {
		// allowed (an internal network), but it is said loudly: the passwords would travel in clear
		c.debugf("WARNING: ldap:// without start_tls: passwords are sent unencrypted")
	}
	return nil
}

func (c *LDAPConfig) debugf(format string, args ...any) {
	if c.Debug != nil {
		c.Debug(fmt.Sprintf(format, args...))
	}
}

// Directory is the LDAP Authenticator.
type Directory struct{ cfg LDAPConfig }

func NewDirectory(cfg LDAPConfig) (*Directory, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &Directory{cfg: cfg}, nil
}

func (d *Directory) tlsConfig() (*tls.Config, error) {
	u, _ := url.Parse(d.cfg.URL)
	conf := &tls.Config{ServerName: u.Hostname(), InsecureSkipVerify: d.cfg.InsecureSkipVerify, MinVersion: tls.VersionTLS12}
	if d.cfg.CAFile != "" {
		pem, err := os.ReadFile(d.cfg.CAFile)
		if err != nil {
			return nil, err
		}
		pool, _ := x509.SystemCertPool()
		if pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("%s has no certificate in PEM format", d.cfg.CAFile)
		}
		conf.RootCAs = pool
	}
	if d.cfg.LegacyTLS {
		conf.MinVersion = tls.VersionTLS10
		var suites []uint16
		for _, s := range tls.CipherSuites() {
			suites = append(suites, s.ID)
		}
		for _, s := range tls.InsecureCipherSuites() {
			suites = append(suites, s.ID)
		}
		conf.CipherSuites = suites
	}
	return conf, nil
}

func (d *Directory) dial() (*ldap.Conn, error) {
	conf, err := d.tlsConfig()
	if err != nil {
		return nil, err
	}
	dialer := &net.Dialer{Timeout: d.cfg.Timeout}
	conn, err := ldap.DialURL(d.cfg.URL, ldap.DialWithDialer(dialer), ldap.DialWithTLSConfig(conf))
	if err != nil {
		return nil, err
	}
	conn.SetTimeout(d.cfg.Timeout)
	if d.cfg.StartTLS {
		if err := conn.StartTLS(conf); err != nil {
			conn.Close()
			return nil, fmt.Errorf("StartTLS: %w", err)
		}
	}
	return conn, nil
}

func replace(template string, values map[string]string) string {
	out := template
	for k, v := range values {
		out = strings.ReplaceAll(out, "{"+k+"}", v)
	}
	return out
}

func unavailable(what string, err error) error {
	return fmt.Errorf("%w (%s: %v)", ErrUnavailable, what, err)
}

// Authenticate checks the password of a person and finds their role.
func (d *Directory) Authenticate(ctx context.Context, username, password string) (*User, error) {
	if !ValidUsername(username) || password == "" { // an empty password would be an "unauthenticated bind", which many servers accept
		return nil, ErrBadCredentials
	}
	c := d.cfg
	c.defaults()
	conn, err := d.dial()
	if err != nil {
		return nil, unavailable("connect", err)
	}
	defer conn.Close()
	c.debugf("connected to %s", c.URL)

	bindService := func() error {
		if c.BindDN == "" {
			return conn.UnauthenticatedBind("")
		}
		return conn.Bind(c.BindDN, c.BindPassword)
	}

	// 1. who is it
	var userDN string
	var entry *ldap.Entry
	if c.UserDNTemplate != "" {
		userDN = replace(c.UserDNTemplate, map[string]string{"username": ldap.EscapeDN(username)})
		c.debugf("user DN from the template: %s", userDN)
	} else {
		if err := bindService(); err != nil {
			return nil, unavailable("bind of the service account", err)
		}
		filter := replace(c.UserFilter, map[string]string{"username": ldap.EscapeFilter(username)})
		res, err := conn.Search(ldap.NewSearchRequest(c.UserBase, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 2, int(c.Timeout.Seconds()), false,
			filter, []string{"dn", c.DisplayAttr, c.MemberOfAttr}, nil))
		if err != nil {
			if ldap.IsErrorWithCode(err, ldap.LDAPResultSizeLimitExceeded) {
				c.debugf("more than one entry for %s", filter)
				return nil, ErrBadCredentials
			}
			return nil, unavailable("search for the user", err)
		}
		c.debugf("search %s under %s: %d entr(ies)", filter, c.UserBase, len(res.Entries))
		if len(res.Entries) != 1 {
			return nil, ErrBadCredentials
		}
		entry = res.Entries[0]
		userDN = entry.DN
	}

	// 2. is the password right
	if err := conn.Bind(userDN, password); err != nil {
		if ldap.IsErrorWithCode(err, ldap.LDAPResultInvalidCredentials) || ldap.IsErrorWithCode(err, ldap.LDAPResultInappropriateAuthentication) {
			c.debugf("bind as %s: wrong password", userDN)
			return nil, ErrBadCredentials
		}
		return nil, unavailable("bind as the user", err)
	}
	c.debugf("bind as %s: ok", userDN)

	// 3. what are they allowed to do: the groups are read with the service account (or as the person when there is none)
	if c.BindDN != "" {
		if err := conn.Bind(c.BindDN, c.BindPassword); err != nil {
			return nil, unavailable("bind of the service account", err)
		}
	}
	if entry == nil || len(entry.GetAttributeValues(c.MemberOfAttr)) == 0 {
		res, err := conn.Search(ldap.NewSearchRequest(userDN, ldap.ScopeBaseObject, ldap.NeverDerefAliases, 1, int(c.Timeout.Seconds()), false,
			"(objectClass=*)", []string{c.DisplayAttr, c.MemberOfAttr}, nil))
		if err == nil && len(res.Entries) == 1 {
			entry = res.Entries[0]
		} else if err != nil {
			c.debugf("reading the entry of the user: %v", err)
		}
	}
	groups := map[string]bool{}
	display := username
	if entry != nil {
		if v := entry.GetAttributeValue(c.DisplayAttr); v != "" {
			display = v
		}
		for _, g := range entry.GetAttributeValues(c.MemberOfAttr) {
			groups[g] = true
		}
		c.debugf("%s of the user: %v", c.MemberOfAttr, entry.GetAttributeValues(c.MemberOfAttr))
	}
	if c.GroupBase != "" {
		filter := replace(c.GroupFilter, map[string]string{"dn": ldap.EscapeFilter(userDN), "username": ldap.EscapeFilter(username)})
		res, err := conn.Search(ldap.NewSearchRequest(c.GroupBase, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 0, int(c.Timeout.Seconds()), false,
			filter, []string{"cn"}, nil))
		if err != nil {
			return nil, unavailable("search for the groups", err)
		}
		c.debugf("group search %s under %s: %d group(s)", filter, c.GroupBase, len(res.Entries))
		for _, e := range res.Entries {
			groups[e.DN] = true
		}
	}

	role, matched := roleOf(c.Roles, groups)
	list := make([]string, 0, len(groups))
	for g := range groups {
		list = append(list, g)
	}
	sort.Strings(list)
	c.debugf("groups of %s: %v -> role %q", username, list, role)
	if role == "" {
		return nil, ErrNotAllowed
	}
	return &User{Name: strings.ToLower(username), DisplayName: display, Role: role, Groups: matched, Source: "ldap"}, nil
}

// roleOf gives the highest role for which one of the person's groups is listed. A group matches by its full DN or by its cn.
func roleOf(roles map[Role][]string, groups map[string]bool) (Role, []string) {
	have := map[string]string{} // lower case name -> the group as the directory says
	for g := range groups {
		have[strings.ToLower(g)] = g
		if cn := commonName(g); cn != "" {
			have[strings.ToLower(cn)] = g
		}
	}
	best := Role("")
	var matched []string
	for role, wanted := range roles {
		for _, w := range wanted {
			if g, ok := have[strings.ToLower(strings.TrimSpace(w))]; ok {
				matched = append(matched, g)
				if role.Rank() > best.Rank() {
					best = role
				}
			}
		}
	}
	sort.Strings(matched)
	return best, matched
}

// commonName gives "admins" for "cn=admins,ou=groups,dc=example,dc=com" (or a bare name).
func commonName(dn string) string {
	parsed, err := ldap.ParseDN(dn)
	if err != nil || len(parsed.RDNs) == 0 {
		return ""
	}
	for _, a := range parsed.RDNs[0].Attributes {
		if strings.EqualFold(a.Type, "cn") {
			return a.Value
		}
	}
	return ""
}
