package collector

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kblaskoff/pdu-monitor/server/internal/store"
	"github.com/kblaskoff/pdu-monitor/server/internal/updater"
)

func writeConfig(t *testing.T, text string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestConfigChecks(t *testing.T) {
	good := "server_url: https://pdu.example.com:8443\ntoken_file: /t\nhosts:\n  - name: a1\n    address: 10.0.0.1\n"
	c, err := Load(writeConfig(t, good))
	if err != nil {
		t.Fatal(err)
	}
	h := c.Hosts[0]
	if h.Port != 22 || h.User != "hostinfo-ro" || c.PollSeconds != 60 || c.TopSeconds != 300 || c.DataDir == "" {
		t.Errorf("defaults: %+v %+v", h, c)
	}
	bad := map[string]string{
		"plain http":      strings.Replace(good, "https://pdu.example.com:8443", "http://pdu.example.com:8080", 1),
		"no scheme":       strings.Replace(good, "https://", "", 1),
		"ftp":             strings.Replace(good, "https", "ftp", 1),
		"no token":        strings.Replace(good, "token_file: /t\n", "", 1),
		"no hosts":        "server_url: https://x.example.com\ntoken_file: /t\n",
		"name with space": strings.Replace(good, "a1", "a 1", 1),
		"duplicate":       good + "  - name: a1\n    address: 10.0.0.2\n",
		"no address":      strings.Replace(good, "address: 10.0.0.1", "address: ''", 1),
		"typo":            good + "pol_seconds: 30\n",
		"fast":            good + "poll_seconds: 2\n",
		"bad port":        good + "    port: 99999\n",
	}
	for name, text := range bad {
		if _, err := Load(writeConfig(t, text)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	for _, u := range []string{"http://localhost:8080", "http://127.0.0.1:8080"} { // a lab
		if _, err := Load(writeConfig(t, strings.Replace(good, "https://pdu.example.com:8443", u, 1))); err != nil {
			t.Errorf("%s: %v", u, err)
		}
	}
	if _, err := Load(writeConfig(t, Example)); err != nil {
		t.Errorf("the example does not load: %v", err)
	}
}

func TestKnownKeysAreBoundToTheAddress(t *testing.T) {
	dir := t.TempDir()
	k, err := LoadKnown(dir)
	if err != nil || len(k) != 0 {
		t.Fatal(k, err)
	}
	if err := k.Confirm(dir, "a1", "10.0.0.1", 22, "ssh-ed25519 AAAA1"); err != nil {
		t.Fatal(err)
	}
	if err := k.Confirm(dir, "a2", "10.0.0.2", 2222, "ssh-rsa AAAA2"); err != nil {
		t.Fatal(err)
	}
	k2, _ := LoadKnown(dir)
	if k2.Key("a1", "10.0.0.1", 22) != "ssh-ed25519 AAAA1" || k2.Key("a2", "10.0.0.2", 2222) != "ssh-rsa AAAA2" {
		t.Errorf("not kept: %v", k2)
	}
	if k2.Key("a1", "10.0.0.99", 22) != "" || k2.Key("a1", "10.0.0.1", 2222) != "" {
		t.Error("a key must not follow the host to another address")
	}
	if st, _ := os.Stat(filepath.Join(dir, "known_hosts")); st.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", st.Mode())
	}
}

type fakeRunner struct {
	mu    sync.Mutex
	calls []string
	fail  map[string]error
	out   map[string]string
}

func (f *fakeRunner) Run(_ context.Context, h store.Host, command string) ([]byte, error) {
	f.mu.Lock()
	f.calls = append(f.calls, h.Name+" "+command)
	f.mu.Unlock()
	if err := f.fail[h.Name]; err != nil {
		return nil, err
	}
	if out, ok := f.out[command]; ok {
		return []byte(out), nil
	}
	return nil, errors.New("no such command")
}

const goodStatus = `{"v":1,"host":"h","t":1700000000,"interval":60,"cpu":{"cores":8,"idle_pct":90,"load":[1,1,1]},"vms":{"count":0,"list":[]}}`
const goodTop = `{"v":1,"window":"1h","covered_s":3600,"other_cpu_s":1,"vms":[]}`

func topFor(w string) string { return strings.Replace(goodTop, `"1h"`, `"`+w+`"`, 1) }

type received struct {
	auth string
	rep  report
}

func stub(t *testing.T, status int, reply string) (*httptest.Server, func() []received) {
	t.Helper()
	var mu sync.Mutex
	var got []received
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var rep report
		_ = json.NewDecoder(r.Body).Decode(&rep)
		mu.Lock()
		got = append(got, received{r.Header.Get("Authorization"), rep})
		mu.Unlock()
		w.WriteHeader(status)
		_, _ = w.Write([]byte(reply))
	}))
	t.Cleanup(ts.Close)
	return ts, func() []received { mu.Lock(); defer mu.Unlock(); return append([]received(nil), got...) }
}

func newCollector(t *testing.T, url string, r *fakeRunner) *Collector {
	t.Helper()
	dir := t.TempDir()
	cfg := &Config{ServerURL: url, DataDir: dir, PollSeconds: 60, TopSeconds: 300, Parallel: 2,
		Hosts: []HostConfig{{Name: "a1", Address: "10.0.0.1", Port: 22, User: "u"}, {Name: "a2", Address: "10.0.0.2", Port: 22, User: "u"}, {Name: "a3", Address: "10.0.0.3", Port: 22, User: "u"}}}
	k, _ := LoadKnown(dir)
	_ = k.Confirm(dir, "a1", "10.0.0.1", 22, "ssh-ed25519 K1")
	_ = k.Confirm(dir, "a2", "10.0.0.2", 22, "ssh-ed25519 K2") // a3 is not confirmed
	now := time.Unix(1_700_000_000, 0)
	return &Collector{Cfg: cfg, Runner: r, Token: "secret-token-secret-token", HTTP: http.DefaultClient, Version: "t", Now: func() time.Time { return now }}
}

func TestCycleSendsStatusThenTopsOnlyWhenDue(t *testing.T) {
	ts, got := stub(t, 200, `{"ok":true,"accepted":3,"rejected":[]}`)
	r := &fakeRunner{out: map[string]string{"status": goodStatus, "top 1h": goodTop, "top 6h": topFor("6h"), "top 24h": topFor("24h"), "top 7d": topFor("7d")}, fail: map[string]error{"a2": errors.New("connection refused")}}
	c := newCollector(t, ts.URL, r)
	if err := c.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	reports := got()
	if len(reports) != 2 {
		t.Fatalf("%d posts, want the status and the tops", len(reports))
	}
	if reports[0].auth != "Bearer secret-token-secret-token" {
		t.Errorf("auth %q", reports[0].auth)
	}
	byName := map[string]hostReport{}
	for _, h := range reports[0].rep.Hosts {
		byName[h.Name] = h
	}
	if len(byName["a1"].Status) == 0 || byName["a1"].Error != "" || len(byName["a1"].Tops) != 0 {
		t.Errorf("a1: %+v", byName["a1"])
	}
	if byName["a2"].Error != "connection refused" || len(byName["a2"].Status) != 0 {
		t.Errorf("a2: %+v", byName["a2"])
	}
	if !strings.Contains(byName["a3"].Error, "pdu-collector trust -host a3") {
		t.Errorf("a3: %+v", byName["a3"])
	}
	for _, call := range r.calls {
		if strings.HasPrefix(call, "a3 ") {
			t.Errorf("a host with an unconfirmed key must not be contacted: %s", call)
		}
	}
	tops := reports[1].rep.Hosts
	if len(tops) != 1 || tops[0].Name != "a1" || len(tops[0].Tops) != 4 || len(tops[0].Status) != 0 {
		t.Errorf("tops: %+v", tops)
	}
	// the second cycle a minute later: status only
	c.Now = func() time.Time { return time.Unix(1_700_000_060, 0) }
	if err := c.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := len(got()); n != 3 {
		t.Errorf("%d posts after the second cycle, want 3", n)
	}
	// after top_seconds the tops are read again
	c.Now = func() time.Time { return time.Unix(1_700_000_400, 0) }
	_ = c.Cycle(context.Background())
	if n := len(got()); n != 5 {
		t.Errorf("%d posts after top_seconds, want 5", n)
	}
}

func TestAnswerThatIsNotAReadingIsAnError(t *testing.T) {
	ts, got := stub(t, 200, `{"ok":true}`)
	r := &fakeRunner{out: map[string]string{"status": `{"v":9}`}}
	c := newCollector(t, ts.URL, r)
	_ = c.Cycle(context.Background())
	for _, h := range got()[0].rep.Hosts {
		if h.Name == "a1" && (len(h.Status) != 0 || h.Error == "") {
			t.Errorf("rubbish was passed on: %+v", h)
		}
	}
}

func TestRefusalAndRedirectsAreErrors(t *testing.T) {
	ts, _ := stub(t, 401, `{"error":"the token is not known"}`)
	r := &fakeRunner{out: map[string]string{"status": goodStatus}}
	c := newCollector(t, ts.URL, r)
	err := c.Cycle(context.Background())
	if err == nil || !strings.Contains(err.Error(), "the token is not known") {
		t.Errorf("401: %v", err)
	}
	// a redirect must not carry the token to another place
	other, otherGot := stub(t, 200, `{"ok":true}`)
	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusTemporaryRedirect)
	}))
	defer redir.Close()
	c = newCollector(t, redir.URL, r)
	client, err := NewHTTPClient("")
	if err != nil {
		t.Fatal(err)
	}
	c.HTTP = client
	if err := c.Cycle(context.Background()); err == nil {
		t.Error("a redirect was followed")
	}
	if len(otherGot()) != 0 {
		t.Error("the token reached the other server")
	}
	// the server is down: an error, not a crash
	c = newCollector(t, "http://127.0.0.1:1", r)
	if err := c.Cycle(context.Background()); err == nil || !strings.Contains(err.Error(), "cannot be reached") {
		t.Errorf("down: %v", err)
	}
}

func TestRejectedHostsAreLoggedOnceUntilTheyChange(t *testing.T) {
	ts, _ := stub(t, 200, `{"ok":true,"accepted":2,"rejected":[{"name":"a1","reason":"disabled on the server"}]}`)
	r := &fakeRunner{out: map[string]string{"status": goodStatus}}
	c := newCollector(t, ts.URL, r)
	var logs []string
	c.Log = func(s string) { logs = append(logs, s) }
	_ = c.Cycle(context.Background())
	_ = c.Cycle(context.Background())
	n := 0
	for _, l := range logs {
		if strings.Contains(l, "disabled on the server") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("logged %d times: %v", n, logs)
	}
}

func TestToken(t *testing.T) {
	p := filepath.Join(t.TempDir(), "token")
	_ = os.WriteFile(p, []byte("  abcdefghijklmnopqrstuvwxyz \n"), 0o600)
	if tok, err := ReadToken(p); err != nil || tok != "abcdefghijklmnopqrstuvwxyz" {
		t.Errorf("%q %v", tok, err)
	}
	_ = os.WriteFile(p, []byte("short\n"), 0o600)
	if _, err := ReadToken(p); err == nil {
		t.Error("a short token was accepted")
	}
	if _, err := ReadToken(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("a missing file was accepted")
	}
}

func TestUpdateRequestIsLeftForTheUpdaterAndAcknowledged(t *testing.T) {
	ts, got := stub(t, 200, `{"ok":true,"accepted":2,"rejected":[],"update":"req-1"}`)
	r := &fakeRunner{out: map[string]string{"status": goodStatus}}
	c := newCollector(t, ts.URL, r)
	var logs []string
	c.Log = func(s string) { logs = append(logs, s) }

	// the updater is not installed here: it is said once, nothing is claimed, the server keeps asking
	_ = c.Cycle(context.Background())
	_ = c.Cycle(context.Background())
	if _, err := os.Stat(filepath.Join(c.Cfg.DataDir, updater.RequestFile)); err == nil {
		t.Fatal("a request was written without an updater")
	}
	n := 0
	for _, l := range logs {
		if strings.Contains(l, "updater is not installed") {
			n++
		}
	}
	if n != 1 || got()[len(got())-1].rep.UpdateSeen != "" {
		t.Fatalf("said %d times: %v", n, logs)
	}

	// installed: the file is written once, and the next report says that it was seen
	if err := os.WriteFile(filepath.Join(c.Cfg.DataDir, updater.MarkerFile), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	_ = c.Cycle(context.Background())
	if _, err := os.Stat(filepath.Join(c.Cfg.DataDir, updater.RequestFile)); err != nil {
		t.Fatalf("no request: %v", err)
	}
	_ = c.Cycle(context.Background())
	all := got()
	if all[len(all)-1].rep.UpdateSeen != "req-1" {
		t.Errorf("not acknowledged: %+v", all[len(all)-1].rep)
	}
	// and it is not written again while the server keeps the same id
	_ = os.Remove(filepath.Join(c.Cfg.DataDir, updater.RequestFile))
	_ = c.Cycle(context.Background())
	if _, err := os.Stat(filepath.Join(c.Cfg.DataDir, updater.RequestFile)); err == nil {
		t.Error("the same request was acted on twice")
	}
}
