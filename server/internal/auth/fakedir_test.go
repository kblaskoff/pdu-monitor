package auth

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jimlambrt/gldap"
)

// A small directory server for the tests: it answers binds and searches like OpenLDAP or AD would for the entries given.

type dirEntry struct {
	dn       string
	attrs    map[string][]string
	password string
}

type fakeDirectory struct {
	t         *testing.T
	addr      string
	entries   []dirEntry
	serviceDN string
	servicePW string
	// anonymous searches are refused unless this is true
	allowAnonymous bool
	mu             sync.Mutex
	bound          map[int]string // connection -> DN it is bound as
	binds          []string
	filters        []string
	server         *gldap.Server
	// startTLS, when set, is offered with the StartTLS extended operation on the plain port
	startTLS *tls.Config
}

func freeAddr(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

func startDirectory(t *testing.T, d *fakeDirectory, tlsConf *tls.Config) {
	t.Helper()
	d.t = t
	d.bound = map[int]string{}
	d.addr = freeAddr(t)
	var opts []gldap.Option
	if tlsConf != nil {
		opts = append(opts, gldap.WithTLSConfig(tlsConf))
	}
	s, err := gldap.NewServer()
	if err != nil {
		t.Fatal(err)
	}
	mux, _ := gldap.NewMux()
	_ = mux.Bind(d.handleBind)
	_ = mux.Search(d.handleSearch)
	if d.startTLS != nil {
		_ = mux.ExtendedOperation(func(w *gldap.ResponseWriter, r *gldap.Request) {
			resp := r.NewExtendedResponse(gldap.WithResponseCode(gldap.ResultSuccess))
			resp.SetResponseName(gldap.ExtendedOperationStartTLS)
			_ = w.Write(resp)
			_ = r.StartTLS(d.startTLS)
		}, gldap.ExtendedOperationStartTLS)
	}
	_ = s.Router(mux)
	d.server = s
	go func() { _ = s.Run(d.addr, opts...) }()
	for i := 0; i < 100 && !s.Ready(); i++ {
		time.Sleep(20 * time.Millisecond)
	}
	t.Cleanup(func() { _ = s.Stop() })
}

func (d *fakeDirectory) handleBind(w *gldap.ResponseWriter, r *gldap.Request) {
	resp := r.NewBindResponse(gldap.WithResponseCode(gldap.ResultInvalidCredentials))
	defer func() { _ = w.Write(resp) }()
	m, err := r.GetSimpleBindMessage()
	if err != nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.binds = append(d.binds, m.UserName)
	ok := false
	switch {
	case m.UserName == "" && string(m.Password) == "":
		ok = true
	case m.UserName == d.serviceDN && string(m.Password) == d.servicePW:
		ok = true
	default:
		for _, e := range d.entries {
			if strings.EqualFold(e.dn, m.UserName) && e.password != "" && e.password == string(m.Password) {
				ok = true
			}
		}
	}
	if ok {
		d.bound[r.ConnectionID()] = m.UserName
		resp = r.NewBindResponse(gldap.WithResponseCode(gldap.ResultSuccess))
	}
}

func (d *fakeDirectory) handleSearch(w *gldap.ResponseWriter, r *gldap.Request) {
	m, err := r.GetSearchMessage()
	res := r.NewSearchDoneResponse(gldap.WithResponseCode(gldap.ResultSuccess))
	defer func() { _ = w.Write(res) }()
	if err != nil {
		return
	}
	d.mu.Lock()
	d.filters = append(d.filters, m.Filter)
	bound := d.bound[r.ConnectionID()]
	d.mu.Unlock()
	if bound == "" && !d.allowAnonymous {
		res = r.NewSearchDoneResponse(gldap.WithResponseCode(gldap.ResultInsufficientAccessRights))
		return
	}
	var found []dirEntry
	for _, e := range d.entries {
		if !strings.HasSuffix(strings.ToLower(e.dn), strings.ToLower(m.BaseDN)) {
			continue
		}
		if m.Scope == gldap.BaseObject && !strings.EqualFold(e.dn, m.BaseDN) {
			continue
		}
		if matchFilter(m.Filter, e) {
			found = append(found, e)
		}
	}
	if m.SizeLimit > 0 && int64(len(found)) > m.SizeLimit {
		res = r.NewSearchDoneResponse(gldap.WithResponseCode(gldap.ResultSizeLimitExceeded))
		return
	}
	for _, e := range found {
		attrs := map[string][]string{}
		for k, v := range e.attrs {
			attrs[k] = v
		}
		_ = w.Write(r.NewSearchResponseEntry(e.dn, gldap.WithAttributes(attrs)))
	}
}

// ---- a filter evaluator for (&..) (|..) (!..) and (attr=value), case-insensitive; no wildcards

func matchFilter(f string, e dirEntry) bool {
	ok, _ := evalFilter(strings.TrimSpace(f), e)
	return ok
}

func evalFilter(f string, e dirEntry) (bool, string) {
	if !strings.HasPrefix(f, "(") {
		return false, f
	}
	f = f[1:]
	switch {
	case strings.HasPrefix(f, "&"), strings.HasPrefix(f, "|"):
		op := f[0]
		rest := f[1:]
		result := op == '&'
		for strings.HasPrefix(rest, "(") {
			var ok bool
			ok, rest = evalFilter(rest, e)
			if op == '&' {
				result = result && ok
			} else {
				result = result || ok
			}
		}
		return result, strings.TrimPrefix(rest, ")")
	case strings.HasPrefix(f, "!"):
		ok, rest := evalFilter(f[1:], e)
		return !ok, strings.TrimPrefix(rest, ")")
	}
	end := strings.Index(f, ")")
	for end > 0 && f[end-1] == '\\' { // an escaped bracket is part of the value
		next := strings.Index(f[end+1:], ")")
		if next < 0 {
			break
		}
		end += 1 + next
	}
	item := f[:end]
	attr, value, _ := strings.Cut(item, "=")
	value = unescapeFilter(value)
	if strings.Contains(value, "*") {
		return attr == "objectClass" && value == "*" || (value == "*" && len(e.attrs[attr]) > 0), f[end+1:]
	}
	for _, v := range e.attrs[attr] {
		if strings.EqualFold(v, value) {
			return true, f[end+1:]
		}
	}
	if strings.EqualFold(attr, "objectClass") && value == "*" {
		return true, f[end+1:]
	}
	return false, f[end+1:]
}

func unescapeFilter(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+2 < len(s)+0 && i+2 <= len(s)-1+1 {
			var c byte
			if _, err := fmt.Sscanf(s[i+1:i+3], "%02x", &c); err == nil {
				b.WriteByte(c)
				i += 2
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// ---- certificates

func selfSigned(t *testing.T, host string) (tlsConf *tls.Config, caFile string) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: host}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true, IsCA: true, DNSNames: []string{host}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	caFile = filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}, caFile
}

// openLDAPDirectory is a directory like the ones of the Linux world: groupOfNames with member, posixGroup with memberUid.
func openLDAPDirectory() *fakeDirectory {
	base := "dc=example,dc=com"
	people := "ou=people," + base
	return &fakeDirectory{serviceDN: "cn=svc,ou=services," + base, servicePW: "svc-pw", entries: []dirEntry{
		{dn: "uid=anna," + people, password: "anna-pw", attrs: map[string][]string{"uid": {"anna"}, "cn": {"Anna Petrova"}, "objectClass": {"inetOrgPerson"}}},
		{dn: "uid=boris," + people, password: "boris-pw", attrs: map[string][]string{"uid": {"boris"}, "cn": {"Boris Ivanov"}, "objectClass": {"inetOrgPerson"}}},
		{dn: "uid=clara," + people, password: "clara-pw", attrs: map[string][]string{"uid": {"clara"}, "cn": {"Clara"}, "objectClass": {"inetOrgPerson"}}},
		{dn: "uid=twin," + people, password: "t1", attrs: map[string][]string{"uid": {"twin"}, "cn": {"Twin"}}},
		{dn: "uid=twin2," + people, password: "t2", attrs: map[string][]string{"uid": {"twin"}, "cn": {"Twin"}}},
		{dn: "cn=admins,ou=groups," + base, attrs: map[string][]string{"cn": {"admins"}, "objectClass": {"groupOfNames"}, "member": {"uid=anna," + people}}},
		{dn: "cn=ops,ou=groups," + base, attrs: map[string][]string{"cn": {"ops"}, "objectClass": {"posixGroup"}, "memberUid": {"boris"}}},
		{dn: "cn=staff,ou=groups," + base, attrs: map[string][]string{"cn": {"staff"}, "objectClass": {"groupOfUniqueNames"}, "uniqueMember": {"uid=anna," + people, "uid=boris," + people, "uid=clara," + people}}},
	}}
}

func openLDAPConfig(d *fakeDirectory, scheme string) LDAPConfig {
	base := "dc=example,dc=com"
	return LDAPConfig{URL: scheme + "://" + d.addr, BindDN: d.serviceDN, BindPassword: d.servicePW, UserBase: "ou=people," + base, UserFilter: "(uid={username})",
		GroupBase: "ou=groups," + base, Roles: map[Role][]string{RoleAdmin: {"admins"}, RoleOperator: {"ops"}}, Timeout: 3 * time.Second}
}
