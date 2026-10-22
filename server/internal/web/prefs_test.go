package web_test

import "testing"

func TestEveryPersonChoosesHowCPUIdleIsShown(t *testing.T) {
	e := newEnv(t)
	anna, vera := e.login("anna"), e.login("vera")
	_, info := anna.call("GET", "/api/session", nil)
	if obj(info["prefs"])["cpuIdleMode"] != "raw" {
		t.Fatalf("the default is what the host reports: %v", info["prefs"])
	}
	if status, out := anna.call("PUT", "/api/me/prefs", map[string]string{"cpuIdleMode": "threads"}); status != 200 || out["cpuIdleMode"] != "threads" {
		t.Fatalf("%d %v", status, out)
	}
	_, info = anna.call("GET", "/api/session", nil)
	if obj(info["prefs"])["cpuIdleMode"] != "threads" {
		t.Errorf("not kept: %v", info["prefs"])
	}
	// another person is not touched, and a new login of the same person has the choice
	_, other := vera.call("GET", "/api/session", nil)
	if obj(other["prefs"])["cpuIdleMode"] != "raw" {
		t.Errorf("the choice of one person reached another: %v", other["prefs"])
	}
	again := e.login("anna")
	_, info = again.call("GET", "/api/session", nil)
	if obj(info["prefs"])["cpuIdleMode"] != "threads" {
		t.Errorf("a new login lost the choice: %v", info["prefs"])
	}
	// only what exists is accepted
	for _, bad := range []map[string]string{{"cpuIdleMode": "<script>"}, {"cpuIdleMode": ""}, {"somethingElse": "raw"}, {"cpuIdleMode": "raw", "x": "y"}} {
		if status, _ := anna.call("PUT", "/api/me/prefs", bad); status != 400 {
			t.Errorf("%v: %d", bad, status)
		}
	}
	if _, info = anna.call("GET", "/api/session", nil); obj(info["prefs"])["cpuIdleMode"] != "threads" {
		t.Error("a refused request changed something")
	}
	if status, _ := e.anonymous().call("PUT", "/api/me/prefs", map[string]string{"cpuIdleMode": "raw"}, true); status != 401 {
		t.Errorf("without a login: %d", status)
	}
	if status, _ := anna.call("PUT", "/api/me/prefs", map[string]string{"cpuIdleMode": "raw"}, true); status != 403 {
		t.Errorf("without the CSRF header: %d", status)
	}
}
