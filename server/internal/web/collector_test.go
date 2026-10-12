package web_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/kblaskoff/pdu-monitor/server/internal/collector"
	"github.com/kblaskoff/pdu-monitor/server/internal/store"
	"github.com/kblaskoff/pdu-monitor/server/internal/web"
)

const tokenA = "token-of-collector-a-0123456789"
const tokenB = "token-of-collector-b-0123456789"

func withCollectors(e *env) {
	e.srv.Collectors = []web.CollectorAuth{
		{Name: "collector-a", Hash: sha256.Sum256([]byte(tokenA))},
		{Name: "collector-b", Hash: sha256.Sum256([]byte(tokenB))},
	}
}

func push(e *env, token string, body any) (int, map[string]any) {
	headers := map[string]string{"Content-Type": "application/json"}
	if token != "" {
		headers["Authorization"] = "Bearer " + token
	}
	resp, data := e.anonymous().raw("POST", "/api/collector/report", body, headers)
	var out map[string]any
	_ = json.Unmarshal(data, &out)
	return resp.StatusCode, out
}

// agentAnswers returns what a real agent would print for a host of this name.
func agentAnswers(t *testing.T, e *env, name string) (status json.RawMessage, tops map[string]json.RawMessage) {
	t.Helper()
	r := e.lab.Runner()
	h := store.Host{Name: name}
	raw, err := r.Run(context.Background(), h, "status")
	if err != nil {
		t.Fatal(err)
	}
	tops = map[string]json.RawMessage{}
	for _, w := range []string{"1h", "6h", "24h", "7d"} {
		out, err := r.Run(context.Background(), h, "top "+w)
		if err != nil {
			t.Fatal(err)
		}
		tops[w] = out
	}
	return raw, tops
}

func TestCollectorNeedsItsToken(t *testing.T) {
	e := newEnv(t)
	withCollectors(e)
	for _, token := range []string{"", "wrong-token-wrong-token-1234", "short", tokenA + "x"} {
		if status, _ := push(e, token, map[string]any{"hosts": []any{}}); status != 401 {
			t.Errorf("token %q: %d", token, status)
		}
	}
	if status, _ := push(e, tokenA, map[string]any{"hosts": []any{}}); status != 200 {
		t.Errorf("right token: %d", status)
	}
	// no collectors configured: nothing is accepted, whatever the token
	e.srv.Collectors = nil
	if status, _ := push(e, tokenA, map[string]any{"hosts": []any{}}); status != 401 {
		t.Errorf("no collectors: %d", status)
	}
}

func TestCollectorWrongTokensAreSlowedDown(t *testing.T) {
	e := newEnv(t)
	withCollectors(e)
	var last int
	for i := 0; i < 40; i++ {
		last, _ = push(e, "wrong-token-wrong-token-1234", map[string]any{})
	}
	if last != 429 {
		t.Errorf("after many wrong tokens: %d", last)
	}
	if status, _ := push(e, tokenA, map[string]any{"hosts": []any{}}); status != 429 {
		t.Errorf("the address stays locked for the right token too: %d", status)
	}
}

func TestCollectorHostsAreCreatedAndShown(t *testing.T) {
	e := newEnv(t)
	withCollectors(e)
	status, tops := agentAnswers(t, e, "c01sf")
	code, out := push(e, tokenA, map[string]any{"version": "1.2.3", "hosts": []any{
		map[string]any{"name": "c01sf", "status": status, "tops": tops},
		map[string]any{"name": "c02sf", "error": "the host does not answer\x07"},
	}})
	if code != 200 || out["accepted"] != float64(2) || len(list(out["rejected"])) != 0 {
		t.Fatalf("%d %v", code, out)
	}
	admin := e.login("anna")
	_, cfg := admin.call("GET", "/api/admin/config", nil)
	cols := list(cfg["collectors"])
	if len(cols) != 2 || obj(cols[0])["name"] != "collector-a" || obj(cols[0])["lastSeen"] == nil || obj(cols[0])["hosts"] != float64(2) || obj(cols[0])["version"] != "1.2.3" || obj(cols[1])["lastSeen"] != nil {
		t.Errorf("collectors: %v", cols)
	}
	_, hosts := e.login("vera").call("GET", "/api/hosts", nil)
	var c01, c02 map[string]any
	for _, h := range list(hosts["hosts"]) {
		switch obj(h)["name"] {
		case "c01sf":
			c01 = obj(h)
		case "c02sf":
			c02 = obj(h)
		}
	}
	if c01 == nil || c01["collector"] != "collector-a" || c01["online"] != true || c01["trusted"] != true || c01["vms"] == nil {
		t.Errorf("c01sf: %v", c01)
	}
	if c02 == nil || c02["online"] != false || !strings.HasPrefix(c02["error"].(string), "the host does not answer") || strings.ContainsRune(c02["error"].(string), '\a') {
		t.Errorf("c02sf: %v", c02)
	}
	// the busiest VMs come from what the collector pushed
	vera := e.login("vera")
	code, top := vera.call("GET", "/api/hosts/"+itoa(int(c01["id"].(float64)))+"/top?window=24h", nil)
	if code != 200 || len(list(top["vms"])) == 0 {
		t.Errorf("top: %d %v", code, top)
	}
	code, top = vera.call("GET", "/api/hosts/"+itoa(int(c02["id"].(float64)))+"/top?window=24h", nil)
	if code != 409 {
		t.Errorf("a host without pushed tops: %d %v", code, top)
	}
	// the key is confirmed at the collector, not here
	code, _ = admin.call("POST", "/api/admin/hosts/"+itoa(int(c01["id"].(float64)))+"/fetchkey", map[string]any{})
	if code != 400 {
		t.Errorf("fetchkey of a collected host: %d", code)
	}
	// editing a host does not hand it to somebody else
	code, saved := admin.call("PUT", "/api/admin/hosts/"+itoa(int(c01["id"].(float64))), map[string]any{"name": "c01sf", "outletKey": "100U22", "enabled": true, "collector": ""})
	if code != 200 || saved["collector"] != "collector-a" || saved["outletKey"] != "100U22" {
		t.Errorf("edit: %d %v", code, saved)
	}
	// the creation is in the log
	_, audit := admin.call("GET", "/api/admin/audit", nil)
	found := false
	for _, a := range list(audit["entries"]) {
		if obj(a)["action"] == "host.discovered" && obj(a)["user"] == "collector:collector-a" {
			found = true
		}
	}
	if !found {
		t.Error("host.discovered is not in the log")
	}
}

func TestCollectorCannotTakeOverHosts(t *testing.T) {
	e := newEnv(t)
	withCollectors(e)
	status, _ := agentAnswers(t, e, "a39sf")
	// a39sf is a host that the server reads itself (demo data)
	code, out := push(e, tokenA, map[string]any{"hosts": []any{map[string]any{"name": "a39sf", "status": status}}})
	if code != 200 || out["accepted"] != float64(0) || len(list(out["rejected"])) != 1 {
		t.Errorf("a direct host: %v", out)
	}
	// a host of collector A cannot be fed by collector B
	push(e, tokenA, map[string]any{"hosts": []any{map[string]any{"name": "c05sf", "status": status}}})
	_, out = push(e, tokenB, map[string]any{"hosts": []any{map[string]any{"name": "c05sf", "status": status}}})
	if out["accepted"] != float64(0) || len(list(out["rejected"])) != 1 {
		t.Errorf("another collector's host: %v", out)
	}
	// rubbish as a status is shown as an error, not as a reading
	_, out = push(e, tokenA, map[string]any{"hosts": []any{map[string]any{"name": "c06sf", "status": map[string]any{"v": 7}}}})
	if out["accepted"] != float64(1) {
		t.Errorf("rubbish: %v", out)
	}
	_, hosts := e.login("vera").call("GET", "/api/hosts", nil)
	for _, h := range list(hosts["hosts"]) {
		if obj(h)["name"] == "c06sf" && (obj(h)["online"] == true || obj(h)["error"] == "") {
			t.Errorf("rubbish was taken as a reading: %v", h)
		}
	}
}

func TestCollectorDisabledHostKeepsNothing(t *testing.T) {
	e := newEnv(t)
	withCollectors(e)
	status, _ := agentAnswers(t, e, "c07sf")
	push(e, tokenA, map[string]any{"hosts": []any{map[string]any{"name": "c07sf", "status": status}}})
	admin := e.login("anna")
	_, hosts := admin.call("GET", "/api/hosts", nil)
	var id any
	for _, h := range list(hosts["hosts"]) {
		if obj(h)["name"] == "c07sf" {
			id = obj(h)["id"]
		}
	}
	admin.call("PUT", "/api/admin/hosts/"+itoa(int(id.(float64))), map[string]any{"name": "c07sf", "enabled": false})
	_, out := push(e, tokenA, map[string]any{"hosts": []any{map[string]any{"name": "c07sf", "status": status}}})
	if out["accepted"] != float64(0) || len(list(out["rejected"])) != 1 {
		t.Errorf("a disabled host: %v", out)
	}
}

func TestCollectorLimits(t *testing.T) {
	e := newEnv(t)
	withCollectors(e)
	hosts := make([]any, 501)
	for i := range hosts {
		hosts[i] = map[string]any{"name": "h" + strconv.Itoa(i)}
	}
	if code, _ := push(e, tokenA, map[string]any{"hosts": hosts}); code != 400 {
		t.Errorf("too many hosts: %d", code)
	}
	resp, _ := e.anonymous().raw("POST", "/api/collector/report", []byte("{not json"), map[string]string{"Authorization": "Bearer " + tokenA})
	if resp.StatusCode != 400 {
		t.Errorf("not JSON: %d", resp.StatusCode)
	}
	if resp, _ := e.anonymous().raw("GET", "/api/collector/report", nil, map[string]string{"Authorization": "Bearer " + tokenA}); resp.StatusCode == 200 {
		t.Error("GET must not be an answer")
	}
}

// The real collector against the real server: what is read at the "hypervisors" ends up on the screen.
func TestCollectorEndToEnd(t *testing.T) {
	e := newEnv(t)
	withCollectors(e)
	dir := t.TempDir()
	cfg := &collector.Config{ServerURL: e.ts.URL, DataDir: dir, PollSeconds: 60, TopSeconds: 300, Parallel: 4,
		Hosts: []collector.HostConfig{{Name: "e01sf", Address: "10.9.0.1", Port: 22, User: "hostinfo-ro"}, {Name: "e02sf", Address: "10.9.0.2", Port: 22, User: "hostinfo-ro"}}}
	known, _ := collector.LoadKnown(dir)
	if err := known.Confirm(dir, "e01sf", "10.9.0.1", 22, "ssh-ed25519 AAAA-test"); err != nil { // e02sf is not confirmed
		t.Fatal(err)
	}
	var logs []string
	c := &collector.Collector{Cfg: cfg, Runner: e.lab.Runner(), Token: tokenA, HTTP: e.ts.Client(), Version: "test", Log: func(s string) { logs = append(logs, s) }}
	if err := c.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, hosts := e.login("vera").call("GET", "/api/hosts", nil)
	got := map[string]map[string]any{}
	for _, h := range list(hosts["hosts"]) {
		got[obj(h)["name"].(string)] = obj(h)
	}
	if got["e01sf"]["online"] != true || got["e01sf"]["collector"] != "collector-a" {
		t.Errorf("e01sf: %v", got["e01sf"])
	}
	if got["e02sf"]["online"] == true || !strings.Contains(got["e02sf"]["error"].(string), "not confirmed") {
		t.Errorf("e02sf: %v", got["e02sf"])
	}
	code, top := e.login("vera").call("GET", "/api/hosts/"+itoa(int(got["e01sf"]["id"].(float64)))+"/top?window=1h", nil)
	if code != 200 || len(list(top["vms"])) == 0 {
		t.Errorf("top after the first cycle: %d %v", code, top)
	}
	// a wrong token is an error the person at the collector can read
	c.Token = "wrong-token-wrong-token-1234"
	err := c.Cycle(context.Background())
	if err == nil || !strings.Contains(err.Error(), "refused") {
		t.Errorf("wrong token: %v", err)
	}
}
