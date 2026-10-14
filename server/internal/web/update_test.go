package web_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kblaskoff/pdu-monitor/server/internal/updater"
)

func TestUpdateIsForAdministratorsOnly(t *testing.T) {
	e := newEnv(t)
	withCollectors(e)
	e.srv.UpdateDir = t.TempDir()
	for name, c := range map[string]*client{"viewer": e.login("vera"), "operator": e.login("boris"), "nobody": e.anonymous()} {
		for _, call := range [][2]string{{"GET", "/api/admin/update"}, {"POST", "/api/admin/update/server"}, {"POST", "/api/admin/update/collectors"}} {
			status, _ := c.call(call[0], call[1], map[string]any{"confirm": true})
			if status != 403 && status != 401 {
				t.Errorf("%s %s %s: %d", name, call[0], call[1], status)
			}
		}
	}
	anna := e.login("anna")
	if status, _ := anna.call("POST", "/api/admin/update/server", map[string]any{"confirm": true}, true); status != 403 { // no CSRF header
		t.Errorf("without the CSRF header: %d", status)
	}
	if _, err := os.Stat(filepath.Join(e.srv.UpdateDir, updater.RequestFile)); err == nil {
		t.Error("something asked for an update without being allowed to")
	}
}

func TestUpdateTheServer(t *testing.T) {
	e := newEnv(t)
	dir := t.TempDir()
	e.srv.UpdateDir = dir
	anna := e.login("anna")
	status, out := anna.call("GET", "/api/admin/update", nil)
	if status != 200 || obj(out["server"])["available"] != false {
		t.Fatalf("not installed yet: %d %v", status, out)
	}
	if status, out = anna.call("POST", "/api/admin/update/server", map[string]any{"confirm": true}); status != 409 {
		t.Fatalf("without the updater installed: %d %v", status, out)
	}
	if err := os.WriteFile(filepath.Join(dir, updater.MarkerFile), nil, 0o644); err != nil { // what install.sh does
		t.Fatal(err)
	}
	if status, _ = anna.call("POST", "/api/admin/update/server", map[string]any{}); status != 400 {
		t.Errorf("unconfirmed: %d", status)
	}
	if _, err := os.Stat(filepath.Join(dir, updater.RequestFile)); err == nil {
		t.Fatal("an unconfirmed request wrote the file")
	}
	if status, out = anna.call("POST", "/api/admin/update/server", map[string]any{"confirm": true}); status != 202 {
		t.Fatalf("%d %v", status, out)
	}
	if _, err := os.Stat(filepath.Join(dir, updater.RequestFile)); err != nil {
		t.Fatalf("the request is not there: %v", err)
	}
	if status, _ = anna.call("POST", "/api/admin/update/server", map[string]any{"confirm": true}); status != 409 {
		t.Errorf("a second request while the first waits: %d", status)
	}
	_, out = anna.call("GET", "/api/admin/update", nil)
	if sv := obj(out["server"]); sv["pending"] != true || sv["available"] != true {
		t.Errorf("%v", sv)
	}
	log, _ := e.st.AuditLog(context.Background(), 5)
	found := false
	for _, a := range log {
		if a.Action == "update.server" && a.User != "" && a.OK {
			found = true
		}
	}
	if !found {
		t.Errorf("not audited: %+v", log)
	}
	// what the script leaves behind is shown, cleaned
	_ = os.Remove(filepath.Join(dir, updater.RequestFile))
	_ = os.WriteFile(filepath.Join(dir, updater.StatusFile), []byte("failed\t1700000000\tthe new version did not stay up\x1b[31m\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, updater.LogFile), []byte("== start\nbuilding\x00\n"), 0o644)
	_, out = anna.call("GET", "/api/admin/update", nil)
	sv := obj(out["server"])
	if sv["state"] != "failed" || sv["pending"] != false || len(list(sv["log"])) != 2 || sv["detail"] != "the new version did not stay up[31m" {
		t.Errorf("%v", sv)
	}
}

func TestUpdateTheCollectors(t *testing.T) {
	e := newEnv(t)
	withCollectors(e)
	now := time.Now()
	e.srv.Now = func() time.Time { return now }
	anna := e.login("anna")
	report := func(token, version, seen string) string {
		t.Helper()
		status, out := push(e, token, map[string]any{"version": version, "update_seen": seen, "hosts": []any{}})
		if status != 200 {
			t.Fatalf("%d %v", status, out)
		}
		s, _ := out["update"].(string)
		return s
	}
	if id := report(tokenA, "v1", ""); id != "" {
		t.Fatalf("nobody asked: %q", id)
	}
	if status, _ := anna.call("POST", "/api/admin/update/collectors", map[string]any{"confirm": true, "collector": "nope"}); status != 404 {
		t.Errorf("an unknown collector: %d", status)
	}
	if status, _ := anna.call("POST", "/api/admin/update/collectors", map[string]any{"collector": "collector-a"}); status != 400 {
		t.Errorf("unconfirmed: %d", status)
	}
	if status, _ := anna.call("POST", "/api/admin/update/collectors", map[string]any{"confirm": true, "collector": "collector-a"}); status != 202 {
		t.Fatalf("%d", status)
	}
	id := report(tokenA, "v1", "")
	if id == "" {
		t.Fatal("the collector is not told")
	}
	if report(tokenB, "v1", "") != "" {
		t.Error("another collector is told too")
	}
	if report(tokenA, "v1", "") != id {
		t.Error("it must be asked again until it has taken the request")
	}
	_, out := anna.call("GET", "/api/admin/update", nil)
	for _, c := range list(out["collectors"]) {
		if want := obj(c)["name"] == "collector-a"; obj(c)["requested"] != want {
			t.Errorf("requested: %v", c)
		}
	}
	if report(tokenA, "v1", id) != "" {
		t.Error("it has taken it: no more asking")
	}
	if report(tokenA, "v1", "") != "" {
		t.Error("and not asked again after a restart without memory")
	}
	// everybody; the new version is the other way to know that it worked
	anna.call("POST", "/api/admin/update/collectors", map[string]any{"confirm": true})
	if report(tokenB, "v1", "") == "" || report(tokenA, "v1", "") == "" {
		t.Fatal("all collectors are told")
	}
	if report(tokenB, "v2", "") != "" {
		t.Error("a different version means it did update")
	}
	// one that never takes it (no updater installed) is left alone after a while
	if report(tokenA, "v1", "") == "" {
		t.Fatal("still wanted")
	}
	now = now.Add(16 * time.Minute)
	if report(tokenA, "v1", "") != "" {
		t.Error("it gives up after 15 minutes")
	}
	log, _ := e.st.AuditLog(context.Background(), 10)
	n := 0
	for _, a := range log {
		if a.Action == "update.collectors" {
			n++
		}
	}
	if n != 2 {
		t.Errorf("audit entries: %d", n)
	}
}
