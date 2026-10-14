package web_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestServmanNeedsAnAdministrator(t *testing.T) {
	e := newEnv(t)
	e.app.Servman = e.lab.Servman()
	for _, c := range []struct{ who, method, path string }{
		{"", "GET", "/api/admin/servman"}, {"vera", "GET", "/api/admin/servman"}, {"boris", "GET", "/api/admin/servman"},
		{"vera", "POST", "/api/admin/servman/refresh"}, {"boris", "POST", "/api/admin/servman/refresh"},
		{"vera", "POST", "/api/admin/servman/apply"}, {"boris", "POST", "/api/admin/servman/apply"},
	} {
		cl := e.anonymous()
		want := 401
		if c.who != "" {
			cl, want = e.login(c.who), 403
		}
		if status, _ := cl.call(c.method, c.path, map[string]any{"all": true, "confirm": true}); status != want {
			t.Errorf("%s %s %s: %d, want %d", c.who, c.method, c.path, status, want)
		}
	}
	// no CSRF token: refused
	admin := e.login("anna")
	if status, _ := admin.call("POST", "/api/admin/servman/apply", map[string]any{"all": true, "confirm": true}, true); status != 403 {
		t.Errorf("without the CSRF token: %d", status)
	}
	if st := e.app.ServmanStatus(); st.Plan != nil || st.SyncAt != nil {
		t.Errorf("a refused request did something: %+v", st)
	}
}

func TestServmanNotConfigured(t *testing.T) {
	e := newEnv(t)
	admin := e.login("anna")
	status, out := admin.call("GET", "/api/admin/servman", nil)
	if status != 200 || out["configured"] != false {
		t.Errorf("%d %v", status, out)
	}
	for _, path := range []string{"/api/admin/servman/refresh", "/api/admin/servman/apply"} {
		if status, _ := admin.call("POST", path, map[string]any{"all": true, "confirm": true}); status != 404 {
			t.Errorf("%s: %d", path, status)
		}
	}
}

func TestServmanPlanAndApply(t *testing.T) {
	e := newEnv(t)
	e.app.Servman = e.lab.Servman()
	admin := e.login("anna")
	status, out := admin.call("POST", "/api/admin/servman/refresh", map[string]any{})
	if status != 200 || out["configured"] != true || out["planError"] != nil {
		t.Fatalf("%d %v", status, out)
	}
	plan := obj(out["plan"])
	changes := list(plan["changes"])
	if len(changes) != 3 || len(list(plan["warnings"])) != 2 {
		t.Fatalf("%v", plan)
	}
	first := obj(changes[0])
	if first["pdu"] != "P100A" || first["outlet"] != float64(20) || first["desired"] != "100U50" || first["kind"] != "fill" {
		t.Errorf("%v", first)
	}
	if rename := obj(changes[2]); rename["pdu"] != "P200A" || rename["outlet"] != float64(13) || rename["kind"] != "rename" || rename["current"] != "200U03" || rename["desired"] != "200U33" {
		t.Errorf("%v", rename)
	}

	if un := list(plan["unmapped"]); len(un) != 1 || obj(un[0])["pdu"] != "P200B" || obj(un[0])["name"] != "200U03" {
		t.Errorf("an outlet with a name that servman does not know is only reported: %v", un)
	}
	// writing needs the confirmation and a choice
	if status, _ := admin.call("POST", "/api/admin/servman/apply", map[string]any{"all": true}); status != 400 {
		t.Errorf("without confirm: %d", status)
	}
	if status, _ := admin.call("POST", "/api/admin/servman/apply", map[string]any{"confirm": true}); status != 400 {
		t.Errorf("without a choice: %d", status)
	}
	if status, _ := admin.call("POST", "/api/admin/servman/apply", map[string]any{"pdu": "P100B", "outlet": 999, "confirm": true}); status != 400 {
		t.Errorf("a silly outlet: %d", status)
	}
	// one outlet of one PDU
	status, out = admin.call("POST", "/api/admin/servman/apply", map[string]any{"pdu": "P100B", "outlet": 14, "confirm": true})
	if status != 202 {
		t.Fatalf("%d %v", status, out)
	}
	var st map[string]any
	for i := 0; i < 300; i++ {
		_, st = admin.call("GET", "/api/admin/servman", nil)
		if st["writing"] == false && st["result"] != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	res := obj(st["result"])
	if res == nil || res["written"] != float64(1) || res["verified"] != float64(1) || len(list(res["failed"])) != 0 {
		t.Fatalf("%v", st)
	}
	if n := len(list(obj(st["plan"])["changes"])); n != 2 {
		t.Errorf("what is left: %d changes", n)
	}
	// the PDU shows the name, and the log knows who did it
	_, rack := e.login("vera").call("GET", "/api/racks/"+itoa(1), nil)
	if !strings.Contains(jsonOf(rack), `"100U50"`) {
		t.Errorf("the name is not on the PDU: %s", jsonOf(rack))
	}
	_, audit := admin.call("GET", "/api/admin/audit", nil)
	var applied, wrote bool
	for _, a := range list(audit["entries"]) {
		switch obj(a)["action"] {
		case "servman.apply":
			applied = obj(a)["user"] == "anna" && obj(a)["detail"] == "P100B outlet 14"
		case "outlet.names":
			wrote = strings.Contains(obj(a)["detail"].(string), "P100B") && obj(a)["ok"] == true
		}
	}
	if !applied || !wrote {
		t.Errorf("the log: %v", audit["entries"])
	}
}

func TestSettingsOfServmanAndOldPagesDoNotUndoThem(t *testing.T) {
	e := newEnv(t)
	e.app.Servman = e.lab.Servman()
	admin := e.login("anna")
	status, saved := admin.call("PUT", "/api/admin/settings", map[string]any{"pollSeconds": 10, "hostPollSeconds": 60, "warnPercent": 80, "restartDelaySeconds": 8, "sampleRetentionDays": 90,
		"servmanPollMinutes": 5, "servmanSyncMinutes": 30, "servmanNameFormat": "ccid_name"})
	if status != 200 || saved["servmanPollMinutes"] != float64(5) || saved["servmanSyncMinutes"] != float64(30) || saved["servmanNameFormat"] != "ccid_name" {
		t.Fatalf("%d %v", status, saved)
	}
	// a page from before these settings existed sends none of them: they stay
	_, again := admin.call("PUT", "/api/admin/settings", map[string]any{"pollSeconds": 15})
	if again["pollSeconds"] != float64(15) || again["servmanPollMinutes"] != float64(5) || again["servmanSyncMinutes"] != float64(30) || again["servmanNameFormat"] != "ccid_name" {
		t.Errorf("%v", again)
	}
	// the name has the machine name too now
	_, out := admin.call("POST", "/api/admin/servman/refresh", map[string]any{})
	found := false
	for _, c := range list(obj(out["plan"])["changes"]) {
		if obj(c)["pdu"] == "P100A" && obj(c)["desired"] == "100U50 web50" {
			found = true
		}
	}
	if !found {
		t.Errorf("%v", obj(out["plan"])["changes"])
	}
	// a value that is not one of the two formats is not kept
	_, bad := admin.call("PUT", "/api/admin/settings", map[string]any{"servmanNameFormat": "whatever"})
	if bad["servmanNameFormat"] != "ccid" {
		t.Errorf("%v", bad)
	}
}

func jsonOf(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
