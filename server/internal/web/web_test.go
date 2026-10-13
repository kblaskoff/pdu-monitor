package web_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/kblaskoff/pdu-monitor/server/internal/app"
	"github.com/kblaskoff/pdu-monitor/server/internal/auth"
	"github.com/kblaskoff/pdu-monitor/server/internal/demo"
	"github.com/kblaskoff/pdu-monitor/server/internal/hostinfo"
	"github.com/kblaskoff/pdu-monitor/server/internal/store"
	"github.com/kblaskoff/pdu-monitor/server/internal/web"
)

type fakeAuth struct {
	users map[string]*auth.User
	pass  map[string]string
	err   error
}

func (f *fakeAuth) Authenticate(_ context.Context, u, p string) (*auth.User, error) {
	if f.err != nil {
		return nil, f.err
	}
	if _, ok := f.users[u]; ok && f.pass[u] == "no-group" && p == "no-group" {
		return nil, auth.ErrNotAllowed
	}
	if user, ok := f.users[u]; ok && f.pass[u] == p && p != "" {
		c := *user
		return &c, nil
	}
	return nil, auth.ErrBadCredentials
}

type fakeKeys struct{ key ssh.PublicKey }

func (f fakeKeys) FetchHostKey(context.Context, string, int) (ssh.PublicKey, error) {
	return f.key, nil
}

type env struct {
	t    *testing.T
	srv  *web.Server
	ts   *httptest.Server
	app  *app.App
	st   *store.Store
	auth *fakeAuth
	keys fakeKeys
	lab  *demo.Lab
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	key, _ := store.LoadOrCreateKey(filepath.Join(dir, "k"))
	sec, _ := store.NewSecrets(key)
	st, err := store.Open(filepath.Join(dir, "db"), sec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	lab, err := demo.Seed(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	a := app.New(st, hostinfo.NewPoller(st, lab.Runner()), lab.Factory)
	a.Sleep = func(context.Context, time.Duration) error { return nil }
	a.PollPDUs(ctx)
	a.Hosts.PollAll(ctx)
	fa := &fakeAuth{users: map[string]*auth.User{
		"anna":  {Name: "anna", DisplayName: "Anna", Role: auth.RoleAdmin, Source: "ldap"},
		"boris": {Name: "boris", DisplayName: "Boris", Role: auth.RoleOperator, Source: "ldap"},
		"vera":  {Name: "vera", DisplayName: "Vera", Role: auth.RoleViewer, Source: "ldap"},
		"clara": {Name: "clara", Role: auth.RoleViewer, Source: "ldap"},
	}, pass: map[string]string{"anna": "pw", "boris": "pw", "vera": "pw", "clara": "no-group"}}
	srv := web.New(a, st, fa, auth.NewSessions(30*time.Minute, 8*time.Hour))
	srv.FailureDelay = 0
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(priv)
	keys := fakeKeys{key: signer.PublicKey()}
	srv.Keys = keys
	srv.ServerKey = "ssh-ed25519 AAAA-server pdu-monitor"
	srv.Demo = true
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return &env{t: t, srv: srv, ts: ts, app: a, st: st, auth: fa, keys: keys, lab: lab}
}

type client struct {
	e    *env
	http *http.Client
	csrf string
}

func (e *env) anonymous() *client {
	jar, _ := cookiejar.New(nil)
	return &client{e: e, http: &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func (e *env) login(user string) *client {
	c := e.anonymous()
	status, out := c.call("POST", "/api/login", map[string]string{"username": user, "password": "pw"}, true)
	if status != 200 {
		e.t.Fatalf("login %s: %d %v", user, status, out)
	}
	c.csrf, _ = out["csrf"].(string)
	return c
}

func (c *client) raw(method, path string, body any, headers map[string]string) (*http.Response, []byte) {
	var r io.Reader
	if body != nil {
		if b, ok := body.([]byte); ok {
			r = bytes.NewReader(b)
		} else {
			j, _ := json.Marshal(body)
			r = bytes.NewReader(j)
		}
	}
	req, _ := http.NewRequest(method, c.e.ts.URL+path, r)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		c.e.t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp, data
}

// call sends a request with the CSRF token (unless noCSRF) and decodes the JSON object that comes back.
func (c *client) call(method, path string, body any, noCSRF ...bool) (int, map[string]any) {
	h := map[string]string{}
	if c.csrf != "" && len(noCSRF) == 0 {
		h["X-CSRF-Token"] = c.csrf
	}
	resp, data := c.raw(method, path, body, h)
	var out map[string]any
	_ = json.Unmarshal(data, &out)
	return resp.StatusCode, out
}

func list(v any) []any         { l, _ := v.([]any); return l }
func obj(v any) map[string]any { m, _ := v.(map[string]any); return m }

func TestNothingWorksWithoutALogin(t *testing.T) {
	e := newEnv(t)
	c := e.anonymous()
	for _, path := range []string{"/api/overview", "/api/racks/1", "/api/hosts", "/api/admin/config", "/api/ops", "/api/pdus/1"} {
		if status, _ := c.call("GET", path, nil); status != 401 {
			t.Errorf("%s: %d", path, status)
		}
	}
	if status, _ := c.call("POST", "/api/power", map[string]any{}, true); status != 401 {
		t.Errorf("power: %d", status)
	}
	if status, out := c.call("GET", "/api/health", nil); status != 200 || out["ok"] != true {
		t.Errorf("health: %d", status)
	}
	if status, out := c.call("GET", "/api/session", nil); status != 401 || out["demo"] != true {
		t.Errorf("session: %d %v", status, out)
	}
	if status, _ := c.call("GET", "/api/nothing", nil); status != 404 {
		t.Errorf("unknown api call: %d", status)
	}
}

func TestSecurityHeadersAndTheStaticPage(t *testing.T) {
	e := newEnv(t)
	resp, data := e.anonymous().raw("GET", "/", nil, nil)
	if resp.StatusCode != 200 || !strings.Contains(string(data), "PDU Monitor") {
		t.Fatalf("%d", resp.StatusCode)
	}
	h := resp.Header
	if !strings.Contains(h.Get("Content-Security-Policy"), "default-src 'self'") || !strings.Contains(h.Get("Content-Security-Policy"), "frame-ancestors 'none'") ||
		h.Get("X-Content-Type-Options") != "nosniff" || h.Get("X-Frame-Options") != "DENY" || h.Get("Referrer-Policy") != "no-referrer" {
		t.Errorf("headers: %v", h)
	}
	api, _ := e.anonymous().raw("GET", "/api/health", nil, nil)
	if api.Header.Get("Cache-Control") != "no-store" {
		t.Error("API answers must not be cached")
	}
	if resp, _ := e.anonymous().raw("GET", "/etc/passwd", nil, nil); resp.StatusCode != 404 {
		t.Errorf("a path that is not a file: %d", resp.StatusCode)
	}
	if resp, _ := e.anonymous().raw("GET", "/static/../../go.mod", nil, nil); resp.StatusCode == 200 {
		t.Error("no way out of the static folder")
	}
	for path, kind := range map[string]string{"/static/app.js": "javascript", "/static/style.css": "css"} {
		resp, body := e.anonymous().raw("GET", path, nil, nil)
		if resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Content-Type"), kind) || len(body) < 100 {
			t.Errorf("%s: %d %q", path, resp.StatusCode, resp.Header.Get("Content-Type"))
		}
	}
}

func TestLoginCookieAndLogout(t *testing.T) {
	e := newEnv(t)
	c := e.anonymous()
	if status, out := c.call("POST", "/api/login", map[string]string{"username": "anna", "password": "wrong"}, true); status != 401 || out["error"] == nil {
		t.Fatalf("%d %v", status, out)
	}
	e.srv.Secure = true
	resp, data := c.raw("POST", "/api/login", map[string]string{"username": "anna", "password": "pw"}, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, data)
	}
	var cookie *http.Cookie
	for _, ck := range resp.Cookies() {
		if ck.Name == "pdum_session" {
			cookie = ck
		}
	}
	if cookie == nil || !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteStrictMode || cookie.Path != "/" || len(cookie.Value) < 40 {
		t.Fatalf("cookie: %+v", cookie)
	}
	if resp.Header.Get("Strict-Transport-Security") == "" {
		t.Error("HSTS with TLS")
	}
	var info map[string]any
	_ = json.Unmarshal(data, &info)
	if obj(info["user"])["role"] != "admin" || info["csrf"] == "" {
		t.Fatalf("%v", info)
	}
	// the cookie of this client is "Secure", the test server is plain HTTP: the cookie jar will not send it, so use a header
	req, _ := http.NewRequest("GET", e.ts.URL+"/api/session", nil)
	req.AddCookie(cookie)
	r2, _ := http.DefaultClient.Do(req)
	if r2.StatusCode != 200 {
		t.Fatalf("session: %d", r2.StatusCode)
	}
	c.csrf = info["csrf"].(string)
	req, _ = http.NewRequest("POST", e.ts.URL+"/api/logout", nil)
	req.AddCookie(cookie)
	req.Header.Set("X-CSRF-Token", c.csrf)
	if r3, _ := http.DefaultClient.Do(req); r3.StatusCode != 200 {
		t.Fatalf("logout: %d", r3.StatusCode)
	}
	req, _ = http.NewRequest("GET", e.ts.URL+"/api/session", nil)
	req.AddCookie(cookie)
	if r4, _ := http.DefaultClient.Do(req); r4.StatusCode != 401 {
		t.Fatalf("the session must be gone after the logout: %d", r4.StatusCode)
	}
}

func TestWrongPasswordsAreSlowedDown(t *testing.T) {
	e := newEnv(t)
	c := e.anonymous()
	for i := 0; i < 6; i++ {
		if status, _ := c.call("POST", "/api/login", map[string]string{"username": "vera", "password": "x"}, true); status != 401 {
			t.Fatalf("attempt %d: %d", i, status)
		}
	}
	resp, _ := c.raw("POST", "/api/login", map[string]string{"username": "vera", "password": "pw"}, nil)
	if resp.StatusCode != 429 || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("the 7th attempt, even with the right password, must wait: %d", resp.StatusCode)
	}
	// another user is not affected
	if status, _ := c.call("POST", "/api/login", map[string]string{"username": "anna", "password": "pw"}, true); status != 200 {
		t.Fatalf("another user: %d", status)
	}
	log, _ := e.st.AuditLog(context.Background(), 20)
	failed := 0
	for _, l := range log {
		if l.Action == "login.failed" && l.User == "vera" && !l.OK {
			failed++
		}
	}
	if failed != 6 {
		t.Fatalf("%d failed logins in the audit log", failed)
	}
}

func TestGroupAndDirectoryProblemsAreDistinguished(t *testing.T) {
	e := newEnv(t)
	c := e.anonymous()
	status, out := c.call("POST", "/api/login", map[string]string{"username": "clara", "password": "no-group"}, true)
	if status != 403 || !strings.Contains(out["error"].(string), "group") {
		t.Fatalf("%d %v", status, out)
	}
	e.auth.err = auth.ErrUnavailable
	status, out = c.call("POST", "/api/login", map[string]string{"username": "anna", "password": "pw"}, true)
	if status != 503 || !strings.Contains(out["error"].(string), "not available") {
		t.Fatalf("%d %v", status, out)
	}
	if status, _ := c.call("POST", "/api/login", []byte("not json"), true); status != 400 {
		t.Fatalf("bad body: %d", status)
	}
}

func TestChangesNeedTheCSRFTokenAndTheSameOrigin(t *testing.T) {
	e := newEnv(t)
	c := e.login("anna")
	body := map[string]any{"name": "R1", "maxAmps": 24}
	if status, _ := c.call("POST", "/api/admin/racks", body, true); status != 403 {
		t.Errorf("no token: %d", status)
	}
	resp, _ := c.raw("POST", "/api/admin/racks", body, map[string]string{"X-CSRF-Token": "wrong"})
	if resp.StatusCode != 403 {
		t.Errorf("wrong token: %d", resp.StatusCode)
	}
	for name, h := range map[string]map[string]string{
		"foreign origin":   {"X-CSRF-Token": c.csrf, "Origin": "https://evil.example"},
		"cross-site fetch": {"X-CSRF-Token": c.csrf, "Sec-Fetch-Site": "cross-site"},
	} {
		if resp, _ := c.raw("POST", "/api/admin/racks", body, h); resp.StatusCode != 403 {
			t.Errorf("%s: %d", name, resp.StatusCode)
		}
	}
	if resp, _ := c.raw("POST", "/api/admin/racks", body, map[string]string{"X-CSRF-Token": c.csrf, "Origin": e.ts.URL}); resp.StatusCode != 200 {
		t.Errorf("a proper request: %d", resp.StatusCode)
	}
	// a login cannot be forged from another site either
	if resp, _ := e.anonymous().raw("POST", "/api/login", map[string]string{"username": "anna", "password": "pw"}, map[string]string{"Origin": "https://evil.example"}); resp.StatusCode != 403 {
		t.Errorf("login from another site: %d", resp.StatusCode)
	}
}

func TestRolesDecideWhatIsAllowed(t *testing.T) {
	e := newEnv(t)
	viewer, operator, admin := e.login("vera"), e.login("boris"), e.login("anna")
	power := map[string]any{"action": "off", "confirm": true, "targets": []map[string]int{{"pdu": 1, "outlet": 1}}}
	if status, _ := viewer.call("GET", "/api/overview", nil); status != 200 {
		t.Error("a viewer looks")
	}
	if status, _ := viewer.call("POST", "/api/power", power); status != 403 {
		t.Errorf("a viewer must not switch: %d", status)
	}
	if status, _ := viewer.call("PUT", "/api/racks/1/labels", map[string]string{"key": "x", "label": "y"}); status != 403 {
		t.Errorf("a viewer must not rename: %d", status)
	}
	for _, path := range []string{"/api/admin/config", "/api/admin/audit", "/api/admin/settings"} {
		for name, c := range map[string]*client{"viewer": viewer, "operator": operator} {
			if status, _ := c.call("GET", path, nil); status != 403 {
				t.Errorf("%s may not read %s: %d", name, path, status)
			}
		}
		if status, _ := admin.call("GET", path, nil); status != 200 {
			t.Errorf("admin %s: %d", path, status)
		}
	}
	if status, _ := operator.call("POST", "/api/admin/racks", map[string]any{"name": "X", "maxAmps": 24}); status != 403 {
		t.Errorf("an operator must not edit the configuration: %d", status)
	}
}

func TestOverviewShowsTheRacksTheOverLimitOneAndTheHosts(t *testing.T) {
	e := newEnv(t)
	c := e.login("vera")
	status, out := c.call("GET", "/api/overview", nil)
	if status != 200 {
		t.Fatalf("%d", status)
	}
	racks := list(out["racks"])
	if len(racks) != 4 {
		t.Fatalf("%d racks", len(racks))
	}
	over := map[string]string{}
	for _, r := range racks {
		sum := obj(obj(r)["summary"])
		over[obj(r)["name"].(string)] = sum["attention"].(string)
	}
	if over["20F"] != "over" || over["100"] != "ok" {
		t.Fatalf("%v", over)
	}
	if obj(out["totals"])["over"].(float64) != 1 || obj(out["totals"])["amps"].(float64) < 50 {
		t.Fatalf("%v", out["totals"])
	}
	hosts := obj(out["hosts"])
	if hosts["total"].(float64) != 15 || hosts["online"].(float64) != 15 || hosts["degraded"].(float64) != 1 {
		t.Fatalf("%v", hosts)
	}
}

func TestRackPageJoinsThePortsOfBothPDUsAndLinksTheHost(t *testing.T) {
	e := newEnv(t)
	c := e.login("vera")
	_, ov := c.call("GET", "/api/overview", nil)
	var id float64
	for _, r := range list(ov["racks"]) {
		if obj(r)["name"] == "200" {
			id = obj(r)["id"].(float64)
		}
	}
	status, out := c.call("GET", "/api/racks/"+itoa(int(id)), nil)
	if status != 200 {
		t.Fatalf("%d", status)
	}
	servers := list(out["servers"])
	if len(servers) != 13 {
		t.Fatalf("%d servers", len(servers))
	}
	var u20 map[string]any
	for _, s := range servers {
		if obj(s)["name"] == "200U20" {
			u20 = obj(s)
		}
	}
	if u20 == nil || len(list(u20["ports"])) != 2 || u20["power"] != "on" {
		t.Fatalf("%v", u20)
	}
	if h := obj(u20["host"]); h == nil || h["name"] != "a41sf" || h["online"] != true {
		t.Fatalf("the hypervisor behind 200U20: %v", u20["host"])
	}
	for _, s := range servers {
		if obj(s)["name"] == "N200U43" && obj(s)["kind"] != "network" {
			t.Error("N200U43 is network gear")
		}
	}
	if status, _ := c.call("GET", "/api/racks/9999", nil); status != 404 {
		t.Error("unknown rack")
	}
	if status, _ := c.call("GET", "/api/racks/abc", nil); status != 404 {
		t.Error("bad id")
	}
	// an operator can give a server a name
	op := e.login("boris")
	if status, _ := op.call("PUT", "/api/racks/"+itoa(int(id))+"/labels", map[string]string{"key": "200U20", "label": "hypervisor 41"}); status != 200 {
		t.Fatalf("label: %d", status)
	}
	_, out = c.call("GET", "/api/racks/"+itoa(int(id)), nil)
	found := false
	for _, s := range list(out["servers"]) {
		if obj(s)["name"] == "200U20" && obj(s)["label"] == "hypervisor 41" {
			found = true
		}
	}
	if !found {
		t.Error("the name was not kept")
	}
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func TestHostsWithLVMAndTheFilesystems(t *testing.T) {
	e := newEnv(t)
	c := e.login("vera")
	_, out := c.call("GET", "/api/hosts", nil)
	byName := map[string]map[string]any{}
	for _, h := range list(out["hosts"]) {
		byName[obj(h)["name"].(string)] = obj(h)
	}
	lvm := byName["a45sf"]
	if len(list(lvm["lvm"])) != 1 || len(list(lvm["pools"])) != 0 {
		t.Fatalf("an LVM host: %v", lvm)
	}
	if fill := list(obj(list(lvm["lvm"])[0])["fill"]); len(fill) != 1 || obj(fill[0])["data_pct"] != 91.4 {
		t.Errorf("the nearly full thin pool: %v", fill)
	}
	if len(list(byName["a52sf"]["lvm"])) != 0 || len(list(byName["a52sf"]["pools"])) != 1 {
		t.Errorf("a ZFS host: %v", byName["a52sf"])
	}
	if lvm["fs"] != nil {
		t.Error("the list of all hosts does not carry the filesystems")
	}
	_, one := c.call("GET", "/api/hosts/"+itoa(int(lvm["id"].(float64))), nil)
	fs := list(obj(one["host"])["fs"])
	if len(fs) != 3 || obj(fs[0])["mount"] != "/" || obj(fs[2])["mount"] != "/home/solusvm" {
		t.Errorf("filesystems of the host: %v", fs)
	}
}

func TestHostsAndTheTopFiveOfAHost(t *testing.T) {
	e := newEnv(t)
	c := e.login("vera")
	status, out := c.call("GET", "/api/hosts", nil)
	if status != 200 || len(list(out["hosts"])) != 15 {
		t.Fatalf("%d", status)
	}
	var id int
	for _, h := range list(out["hosts"]) {
		hb := obj(h)
		if hb["name"] == "a52sf" {
			id = int(hb["id"].(float64))
			if hb["online"] != true || hb["cores"] == nil || hb["vms"] == nil || len(list(hb["pools"])) != 1 || obj(hb["power"]) == nil {
				t.Fatalf("%v", hb)
			}
			if obj(hb["power"])["server"] != "200U18" {
				t.Fatalf("power link: %v", hb["power"])
			}
		}
	}
	status, out = c.call("GET", "/api/hosts/"+itoa(id), nil)
	if status != 200 || len(list(obj(out["host"])["vmList"])) == 0 {
		t.Fatalf("%d %v", status, out)
	}
	for _, w := range []string{"1h", "6h", "24h", "7d"} {
		status, top := c.call("GET", "/api/hosts/"+itoa(id)+"/top?window="+w, nil)
		if status != 200 || top["window"] != w || len(list(top["vms"])) == 0 {
			t.Fatalf("%s: %d %v", w, status, top)
		}
	}
	if status, _ := c.call("GET", "/api/hosts/"+itoa(id)+"/top?window=3h", nil); status != 400 {
		t.Error("only the known windows")
	}
	if status, _ := c.call("GET", "/api/hosts/"+itoa(id)+"/top?window=6h%3Bid", nil); status != 400 {
		t.Error("only the known windows")
	}
	if status, _ := c.call("GET", "/api/hosts/99999", nil); status != 404 {
		t.Error("unknown host")
	}
}

func TestRestartOfAServerEndToEndWithTheAuditLog(t *testing.T) {
	e := newEnv(t)
	op := e.login("boris")
	_, ov := op.call("GET", "/api/overview", nil)
	var rack float64
	for _, r := range list(ov["racks"]) {
		if obj(r)["name"] == "200" {
			rack = obj(r)["id"].(float64)
		}
	}
	_, out := op.call("GET", "/api/racks/"+itoa(int(rack)), nil)
	var ports []map[string]int
	for _, s := range list(out["servers"]) {
		if obj(s)["name"] == "200U31" {
			for _, p := range list(obj(s)["ports"]) {
				ports = append(ports, map[string]int{"pdu": int(obj(p)["pdu"].(float64)), "outlet": int(obj(p)["outlet"].(float64))})
			}
		}
	}
	if len(ports) != 2 {
		t.Fatalf("%v", ports)
	}
	if status, _ := op.call("POST", "/api/power", map[string]any{"action": "restart", "targets": ports}); status != 400 {
		t.Errorf("without the confirmation: %d", status)
	}
	if status, out := op.call("POST", "/api/power", map[string]any{"action": "format", "confirm": true, "targets": ports}); status != 400 {
		t.Errorf("unknown action: %d %v", status, out)
	}
	status, started := op.call("POST", "/api/power", map[string]any{"action": "restart", "confirm": true, "targets": ports})
	if status != 202 || started["running"] != true {
		t.Fatalf("%d %v", status, started)
	}
	var done map[string]any
	for i := 0; i < 300; i++ {
		_, done = op.call("GET", "/api/ops/"+started["id"].(string), nil)
		if done["running"] == false {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if done["running"] != false || done["error"] != nil || done["detail"] != "Done" {
		t.Fatalf("%v", done)
	}
	if status, ops := op.call("GET", "/api/ops", nil); status != 200 || len(list(ops["operations"])) != 1 {
		t.Fatalf("%d %v", status, ops)
	}
	time.Sleep(50 * time.Millisecond)
	admin := e.login("anna")
	_, audit := admin.call("GET", "/api/admin/audit", nil)
	var restart, finished, login bool
	for _, en := range list(audit["entries"]) {
		m := obj(en)
		switch m["action"] {
		case "power.restart":
			restart = m["user"] == "boris" && strings.Contains(m["detail"].(string), "200U31")
		case "power.restart.done":
			finished = m["ok"] == true
		case "login":
			login = true
		}
	}
	if !restart || !finished || !login {
		t.Fatalf("audit: restart=%v done=%v login=%v", restart, finished, login)
	}
	if status, _ := op.call("GET", "/api/ops/nonexistent", nil); status != 404 {
		t.Error("unknown operation")
	}
}

func TestAdminCanManageRacksPDUsAndTheCommunitiesNeverComeBack(t *testing.T) {
	e := newEnv(t)
	admin := e.login("anna")
	status, rack := admin.call("POST", "/api/admin/racks", map[string]any{"name": "300", "maxAmps": 32})
	if status != 200 || rack["name"] != "300" {
		t.Fatalf("%d %v", status, rack)
	}
	if status, out := admin.call("POST", "/api/admin/racks", map[string]any{"name": "300", "maxAmps": 32}); status != 400 || !strings.Contains(out["error"].(string), "already used") {
		t.Errorf("duplicate: %d %v", status, out)
	}
	pduBody := map[string]any{"rackId": rack["id"], "name": "P300A", "vendor": "apc", "host": "demo-100A", "port": 161, "maxAmps": 24,
		"readCommunity": "SECRET-READ-1", "writeCommunity": "SECRET-WRITE-1"}
	status, saved := admin.call("POST", "/api/admin/pdus", pduBody)
	if status != 200 || saved["hasReadCommunity"] != true || saved["hasWriteCommunity"] != true {
		t.Fatalf("%d %v", status, saved)
	}
	for _, path := range []string{"/api/admin/config", "/api/admin/audit"} {
		resp, data := admin.raw("GET", path, nil, nil)
		if resp.StatusCode != 200 || strings.Contains(string(data), "SECRET") {
			t.Errorf("a community is in %s", path)
		}
	}
	// an edit with empty communities keeps them; the test button can use the stored read community
	id := itoa(int(saved["id"].(float64)))
	pduBody["maxAmps"] = 16
	pduBody["readCommunity"], pduBody["writeCommunity"] = "", ""
	if status, out := admin.call("PUT", "/api/admin/pdus/"+id, pduBody); status != 200 || out["maxAmps"].(float64) != 16 {
		t.Fatalf("%d %v", status, out)
	}
	c, _ := e.st.PDUCommunities(context.Background(), int64(saved["id"].(float64)))
	if c.Read != "SECRET-READ-1" || c.Write != "SECRET-WRITE-1" {
		t.Fatalf("the communities were lost: %+v", c)
	}
	status, test := admin.call("POST", "/api/admin/pdus/test", map[string]any{"id": saved["id"], "vendor": "apc", "host": "demo-100A", "port": 161})
	if status != 200 || test["ok"] != true || test["outlets"].(float64) != 24 {
		t.Fatalf("%d %v", status, test)
	}
	if _, test = admin.call("POST", "/api/admin/pdus/test", map[string]any{"vendor": "apc", "host": "no-such-pdu", "port": 161, "readCommunity": "x"}); test["ok"] != false || test["error"] == nil {
		t.Fatalf("%v", test)
	}
	// validation
	for name, mutate := range map[string]func(map[string]any){
		"vendor": func(b map[string]any) { b["vendor"] = "hp" },
		"host":   func(b map[string]any) { b["host"] = "a b" },
		"port":   func(b map[string]any) { b["port"] = 0 },
		"limit":  func(b map[string]any) { b["maxAmps"] = 0 },
	} {
		b := map[string]any{"rackId": rack["id"], "name": "PX", "vendor": "apc", "host": "h", "port": 161, "maxAmps": 24, "readCommunity": "x"}
		mutate(b)
		if status, _ := admin.call("POST", "/api/admin/pdus", b); status != 400 {
			t.Errorf("%s accepted: %d", name, status)
		}
	}
	if status, _ := admin.call("DELETE", "/api/admin/pdus/"+id, nil); status != 200 {
		t.Error("delete")
	}
	if status, _ := admin.call("DELETE", "/api/admin/pdus/"+id, nil); status != 404 {
		t.Error("second delete")
	}
	if status, _ := admin.call("DELETE", "/api/admin/racks/"+itoa(int(rack["id"].(float64))), nil); status != 200 {
		t.Error("delete rack")
	}
}

func TestTheKeyOfAHostMustBeConfirmedByLookingAtItsFingerprint(t *testing.T) {
	e := newEnv(t)
	admin := e.login("anna")
	status, h := admin.call("POST", "/api/admin/hosts", map[string]any{"name": "new-host", "address": "10.9.9.9", "enabled": true})
	if status != 200 || h["hostKey"] != "" || h["port"].(float64) != 22 || h["user"] != "hostinfo-ro" {
		t.Fatalf("%d %v", status, h)
	}
	id := itoa(int(h["id"].(float64)))
	if status, _ := admin.call("POST", "/api/admin/hosts/"+id+"/trust", map[string]string{"fingerprint": "SHA256:whatever"}); status != 409 {
		t.Errorf("trust before looking: %d", status)
	}
	status, key := admin.call("POST", "/api/admin/hosts/"+id+"/fetchkey", nil)
	if status != 200 || !strings.HasPrefix(key["fingerprint"].(string), "SHA256:") {
		t.Fatalf("%d %v", status, key)
	}
	if status, _ := admin.call("POST", "/api/admin/hosts/"+id+"/trust", map[string]string{"fingerprint": "SHA256:other"}); status != 409 {
		t.Errorf("a different fingerprint: %d", status)
	}
	if status, _ := admin.call("POST", "/api/admin/hosts/"+id+"/trust", map[string]string{"fingerprint": ""}); status != 409 {
		t.Errorf("an empty fingerprint: %d", status)
	}
	if status, _ := admin.call("POST", "/api/admin/hosts/"+id+"/trust", map[string]string{"fingerprint": key["fingerprint"].(string)}); status != 200 {
		t.Fatalf("the right fingerprint")
	}
	stored, _ := e.st.Host(context.Background(), int64(h["id"].(float64)))
	if !strings.HasPrefix(stored.HostKey, "ssh-ed25519 ") {
		t.Fatalf("%q", stored.HostKey)
	}
	// until it is confirmed again the top five of the host is not asked for after an address change
	h["address"], h["hostKey"] = "10.8.8.8", "ignored"
	status, moved := admin.call("PUT", "/api/admin/hosts/"+id, h)
	if status != 200 || moved["hostKey"] != "" {
		t.Fatalf("a new address needs a new confirmation: %d %v", status, moved)
	}
	if status, _ := e.login("vera").call("GET", "/api/hosts/"+id+"/top?window=1h", nil); status != 409 {
		t.Errorf("an unconfirmed host is not asked: %d", status)
	}
}

func TestImportOfTheMacSettingsFileAndSettings(t *testing.T) {
	e := newEnv(t)
	admin := e.login("anna")
	file := `{"format":"pdu-monitor-settings","version":1,"racks":[{"id":"R1","name":"900","maxAmps":20}],
	 "devices":[{"id":"D1","name":"P900A","rackID":"R1","vendor":"apc","host":"10.1.1.1","port":161,"maxAmps":20}],
	 "labels":{"labels":{"R1":{"900u1":"web"}}},"settings":{"pollInterval":20,"restartDelay":9,"warnPercent":75},"communities":{"D1":{"read":"abc"}}}`
	resp, data := admin.raw("POST", "/api/admin/import", []byte(file), map[string]string{"X-CSRF-Token": admin.csrf, "Content-Type": "application/json"})
	if resp.StatusCode != 200 || !strings.Contains(string(data), `"racks":1`) || !strings.Contains(string(data), `"pdus":1`) {
		t.Fatalf("%d %s", resp.StatusCode, data)
	}
	if resp, _ := admin.raw("POST", "/api/admin/import", []byte(`{"hello":1}`), map[string]string{"X-CSRF-Token": admin.csrf}); resp.StatusCode != 400 {
		t.Errorf("a foreign file: %d", resp.StatusCode)
	}
	_, st := admin.call("GET", "/api/admin/settings", nil)
	if st["pollSeconds"].(float64) != 20 || st["warnPercent"].(float64) != 75 || st["restartDelaySeconds"].(float64) != 9 {
		t.Fatalf("%v", st)
	}
	st["pollSeconds"], st["warnPercent"] = 1, 150
	_, saved := admin.call("PUT", "/api/admin/settings", st)
	if saved["pollSeconds"].(float64) != 10 || saved["warnPercent"].(float64) != 80 {
		t.Fatalf("values out of range fall back: %v", saved)
	}
	big := bytes.Repeat([]byte("x"), 5<<20)
	if resp, _ := admin.raw("POST", "/api/admin/import", big, map[string]string{"X-CSRF-Token": admin.csrf}); resp.StatusCode != 400 {
		t.Errorf("a huge body: %d", resp.StatusCode)
	}
}

func TestPDUPageAndHistory(t *testing.T) {
	e := newEnv(t)
	c := e.login("vera")
	_, ov := c.call("GET", "/api/overview", nil)
	pdu := obj(list(obj(list(ov["racks"])[1])["pdus"])[1])
	id := itoa(int(pdu["id"].(float64)))
	status, out := c.call("GET", "/api/pdus/"+id, nil)
	snap := obj(out["snapshot"])
	if status != 200 || len(list(snap["outlets"])) == 0 || out["hasOutletMetering"] == nil {
		t.Fatalf("%d %v", status, out)
	}
	status, hist := c.call("GET", "/api/pdus/"+id+"/history?hours=24", nil)
	if status != 200 || hist["hours"].(float64) != 24 || hist["points"] == nil {
		t.Fatalf("%d %v", status, hist)
	}
	if status, _ := c.call("GET", "/api/pdus/999999", nil); status != 404 {
		t.Error("unknown PDU")
	}
	var _ = errors.New
}

func TestOneUserCannotUseAnotherUsersSession(t *testing.T) {
	e := newEnv(t)
	a, b := e.login("anna"), e.login("vera")
	if status, _ := b.call("GET", "/api/admin/config", nil); status != 403 {
		t.Fatal("the viewer is a viewer")
	}
	// the token of one session does not work with the cookie of the other
	resp, _ := a.raw("POST", "/api/admin/racks", map[string]any{"name": "Z", "maxAmps": 24}, map[string]string{"X-CSRF-Token": b.csrf})
	if resp.StatusCode != 403 {
		t.Fatalf("%d", resp.StatusCode)
	}
}

// A fresh install has no racks yet: the lists must still be lists, the settings page cannot work with null.
func TestEmptyDatabaseGivesEmptyLists(t *testing.T) {
	e := newEnv(t)
	admin := e.login("anna")
	_, cfg := admin.call("GET", "/api/admin/config", nil)
	for _, r := range list(cfg["racks"]) {
		if status, _ := admin.call("DELETE", "/api/admin/racks/"+itoa(int(obj(r)["id"].(float64))), nil); status != 200 {
			t.Fatalf("delete rack: %d", status)
		}
	}
	for _, h := range list(cfg["hosts"]) {
		if status, _ := admin.call("DELETE", "/api/admin/hosts/"+itoa(int(obj(h)["id"].(float64))), nil); status != 200 {
			t.Fatalf("delete host: %d", status)
		}
	}
	_, data := admin.raw("GET", "/api/admin/config", nil, nil)
	var out map[string]json.RawMessage
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"racks", "pdus", "hosts", "collectors"} {
		if string(out[key]) != "[]" {
			t.Errorf("%s: %s, want []", key, out[key])
		}
	}
}
