package servman

import (
	"context"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The shape is what the real API answers (a record of /api/servers/get, with the two PDUs of the server).
const answer = `{"status":true,"data":{
 "2028":{"id":2028,"ccid":"200U13","name":"a42sf","rack_id":61,"rack_position":13,"server_type":"physical","status":"active",
   "netswitch":{"netswitch_id":"1582","netswitch_port":"46"},
   "pdu":[{"pdu_id":1367,"pdu_port":"14","pdu_name":"P200A","pdu_ccid":"P200A"},{"pdu_id":1912,"pdu_port":"14","pdu_name":"P200B","pdu_ccid":"P200B"}]},
 "2029":{"id":2029,"ccid":"200U31","name":"h35sf","status":"active","pdu":"[{\"pdu_id\":1367,\"pdu_port\":1,\"pdu_name\":\"P200A\",\"pdu_ccid\":\"P200A\"}]"},
 "2030":{"id":2030,"ccid":"","name":"no-id","pdu":[{"pdu_port":"3","pdu_ccid":"P200A"}]},
 "2031":{"id":2031,"ccid":"200U40","name":"nopdu","pdu":"[]"},
 "2032":{"id":2032,"ccid":"200U41","name":"odd","pdu":[{"pdu_port":"x","pdu_ccid":"P200A"},{"pdu_port":"0","pdu_ccid":"P200A"},{"pdu_port":"5"},{"pdu_port":"6","pdu_name":"P200B"}]},
 "2033":"not a record"},
 "execution_time":0.2,"ttl":3600}`

func TestParseTheRealShape(t *testing.T) {
	devs, err := ParseDevices([]byte(answer))
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]Device{}
	for _, d := range devs {
		by[d.CCID] = d
	}
	if len(devs) != 4 {
		t.Fatalf("%d devices: %+v", len(devs), devs) // 200U13, 200U31, 200U40, 200U41: no ccid and no record are left out
	}
	d := by["200U13"]
	if d.ID != 2028 || d.Name != "a42sf" || d.Status != "active" || len(d.Ports) != 2 || d.Ports[0] != (Port{"P200A", 14}) || d.Ports[1] != (Port{"P200B", 14}) {
		t.Errorf("%+v", d)
	}
	if p := by["200U31"].Ports; len(p) != 1 || p[0] != (Port{"P200A", 1}) {
		t.Errorf("a list given as a text with a number for the port: %+v", p)
	}
	if len(by["200U40"].Ports) != 0 {
		t.Errorf("%+v", by["200U40"])
	}
	if p := by["200U41"].Ports; len(p) != 1 || p[0] != (Port{"P200B", 6}) { // the name is used when there is no ccid; bad ports are dropped
		t.Errorf("%+v", p)
	}
}

func TestParseOtherShapesAndErrors(t *testing.T) {
	if d, err := ParseDevices([]byte(`{"status":true,"data":[{"id":1,"ccid":"10FU1","pdu":[]}]}`)); err != nil || len(d) != 1 {
		t.Errorf("a list: %v %v", d, err)
	}
	if d, err := ParseDevices([]byte(`{"status":true,"data":{"data":[{"id":1,"ccid":"10FU1"}],"total":1}}`)); err != nil || len(d) != 1 {
		t.Errorf("one more level: %v %v", d, err)
	}
	for name, body := range map[string]string{"not json": "<html>", "status false": `{"status":false,"msg":"bad\u0007token"}`, "no data": `{"status":true}`, "data is text": `{"status":true,"data":"x"}`} {
		if _, err := ParseDevices([]byte(body)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := ParseDevices([]byte(`{"status":false,"msg":"bad\u0007token"}`)); err == nil || strings.ContainsRune(err.Error(), 7) || !strings.Contains(err.Error(), "badtoken") {
		t.Errorf("the message of servman is cleaned: %v", err)
	}
}

func tlsServer(t *testing.T, h http.HandlerFunc) (*httptest.Server, string) {
	t.Helper()
	ts := httptest.NewTLSServer(h)
	t.Cleanup(ts.Close)
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ts.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	return ts, ca
}

func TestClientAsksWithTheTokenAndOnlyReads(t *testing.T) {
	var got []*http.Request
	ts, ca := tlsServer(t, func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Clone(r.Context()))
		_, _ = w.Write([]byte(answer))
	})
	c, err := New(Config{URL: ts.URL, Token: "secret-token-1234", CAFile: ca, Sources: []string{"servers", "netswitches"}})
	if err != nil {
		t.Fatal(err)
	}
	devs, err := c.Devices(context.Background())
	if err != nil || len(devs) != 8 { // the same answer twice
		t.Fatalf("%v %d", err, len(devs))
	}
	if len(got) != 2 || got[0].URL.Path != "/api/servers/get" || got[0].URL.RawQuery != "per_page=all" || got[1].URL.Path != "/api/netswitches/get" {
		t.Fatalf("%v", got)
	}
	for _, r := range got {
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer secret-token-1234" {
			t.Errorf("%s %q", r.Method, r.Header.Get("Authorization"))
		}
	}
}

func TestClientRefusesWhatIsUnsafeAndNeverShowsTheToken(t *testing.T) {
	if _, err := New(Config{URL: "http://servman.example.com", Token: "secret-token-1234"}); err == nil {
		t.Error("http accepted")
	}
	if _, err := New(Config{URL: "https://servman.example.com", Token: "  "}); err == nil {
		t.Error("empty token accepted")
	}
	if _, err := New(Config{URL: "https://servman.example.com", Token: "x", Sources: []string{"clients"}}); err == nil {
		t.Error("an endpoint that is not a list of devices")
	}
	if _, err := New(Config{URL: "https://servman.example.com", Token: "x", CAFile: filepath.Join(t.TempDir(), "missing")}); err == nil {
		t.Error("missing CA file")
	}
	// the certificate of the server is checked: without its CA the connection fails, and the message does not carry the token
	ts, ca := tlsServer(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(answer)) })
	c, _ := New(Config{URL: ts.URL, Token: "secret-token-1234"})
	if _, err := c.Devices(context.Background()); err == nil || strings.Contains(err.Error(), "secret-token-1234") {
		t.Errorf("untrusted certificate: %v", err)
	}
	// ... unless the administrator chose to skip it
	c, _ = New(Config{URL: ts.URL, Token: "secret-token-1234", InsecureSkipVerify: true})
	if devs, err := c.Devices(context.Background()); err != nil || len(devs) != 4 {
		t.Errorf("insecure_skip_verify: %v %d", err, len(devs))
	}
	// a redirect is not followed (the token would go with it), a failure status is an error
	redirect, _ := tlsServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://elsewhere.example.com/", http.StatusFound)
	})
	c, _ = New(Config{URL: redirect.URL, Token: "secret-token-1234", InsecureSkipVerify: true})
	if _, err := c.Devices(context.Background()); err == nil || !strings.Contains(err.Error(), "302") {
		t.Errorf("redirect: %v", err)
	}
	fail, _ := tlsServer(t, func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", http.StatusUnauthorized) })
	c, _ = New(Config{URL: fail.URL, Token: "secret-token-1234", InsecureSkipVerify: true})
	if _, err := c.Devices(context.Background()); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("401: %v", err)
	}
	_ = ca
}

// ------------------------------------------------------------------ the plan

var now = time.Unix(1_760_000_000, 0)

func pduInfo(id int64, name string, count int, names map[int]string) PDUInfo {
	o := map[int]string{}
	for i := 1; i <= count; i++ {
		o[i] = "Outlet_" + string(rune('0'+i%10))
	}
	for n, v := range names {
		o[n] = v
	}
	return PDUInfo{ID: id, Name: name, Read: true, Outlets: o, HasWrite: true, Namer: true}
}

func TestPlanNamesTheOutletsAfterTheDevice(t *testing.T) {
	devs := []Device{
		{ID: 1, CCID: "200U13", Name: "a42sf", Ports: []Port{{"P200A", 14}, {"p200b", 14}}}, // the case of the PDU name does not matter
		{ID: 2, CCID: "200U31", Name: "h35sf", Ports: []Port{{"P200A", 1}}},
		{ID: 3, CCID: "200U40", Name: "x", Ports: []Port{{"P200A", 2}}},
		{ID: 4, CCID: "200U50", Name: "nowhere"}, // no cables: not part of the plan
	}
	pdus := []PDUInfo{
		pduInfo(10, "P200A", 24, map[int]string{1: "200U31", 2: "20FU27", 5: "200U7"}),
		pduInfo(11, "P200B", 24, nil),
	}
	p := Build(devs, pdus, FormatCCID, now)
	if p.Devices != 3 || p.Unchanged != 1 || len(p.Changes) != 3 {
		t.Fatalf("%+v", p)
	}
	want := []struct {
		pdu           string
		outlet        int
		cur, des, knd string
	}{{"P200A", 2, "20FU27", "200U40", "rename"}, {"P200A", 14, "Outlet_4", "200U13", "fill"}, {"P200B", 14, "Outlet_4", "200U13", "fill"}}
	for i, w := range want {
		c := p.Changes[i]
		if c.PDU != w.pdu || c.Outlet != w.outlet || c.Current != w.cur || c.Desired != w.des || c.Kind != w.knd || c.Blocked != "" || c.PDUID == 0 {
			t.Errorf("change %d: %+v, want %+v", i, c, w)
		}
	}
	if len(p.Unmapped) != 1 || p.Unmapped[0] != (Unmapped{"P200A", 5, "200U7"}) { // a name of its own that servman does not know is only reported
		t.Errorf("unmapped: %+v", p.Unmapped)
	}
	if len(p.Warnings) != 0 {
		t.Errorf("%+v", p.Warnings)
	}
}

func TestPlanDoesNotGuess(t *testing.T) {
	devs := []Device{
		{ID: 1, CCID: "200U13", Ports: []Port{{"P200A", 3}}},
		{ID: 2, CCID: "200U14", Ports: []Port{{"P200A", 3}}},               // the same outlet: a conflict, nothing is written
		{ID: 3, CCID: "200U15", Ports: []Port{{"P200A", 99}}},              // no such outlet
		{ID: 4, CCID: "200U16", Ports: []Port{{"P999Z", 1}, {"P999Z", 2}}}, // a PDU that is not in this system
		{ID: 5, CCID: "200U17", Ports: []Port{{"P200C", 1}}},               // a PDU that has not been read
		{ID: 6, CCID: "200U18", Ports: []Port{{"P200A", 4}, {"P200A", 4}}}, // the same cable twice is one cable
		{ID: 7, CCID: "200U19", Ports: []Port{{"P200A", 5}}},
	}
	unread := PDUInfo{ID: 12, Name: "P200C", Read: false}
	pdus := []PDUInfo{pduInfo(10, "P200A", 24, nil), unread}
	p := Build(devs, pdus, FormatCCID, now)
	var changed []string
	for _, c := range p.Changes {
		changed = append(changed, c.Desired)
	}
	if strings.Join(changed, ",") != "200U18,200U19" {
		t.Errorf("changes: %v", changed)
	}
	kinds := map[string]int{}
	for _, w := range p.Warnings {
		kinds[w.Kind]++
	}
	if kinds["conflict"] != 1 || kinds["out-of-range"] != 1 || kinds["unknown-pdu"] != 1 || kinds["not-read"] != 1 || len(p.Warnings) != 4 {
		t.Errorf("%+v", p.Warnings)
	}
}

func TestPlanThreeDevicesOnOneOutletIsOneConflict(t *testing.T) {
	devs := []Device{{ID: 1, CCID: "a1", Ports: []Port{{"P1", 1}}}, {ID: 2, CCID: "a2", Ports: []Port{{"P1", 1}}}, {ID: 3, CCID: "a3", Ports: []Port{{"P1", 1}}}}
	p := Build(devs, []PDUInfo{pduInfo(1, "P1", 4, nil)}, FormatCCID, now)
	if len(p.Changes) != 0 || len(p.Warnings) != 1 {
		t.Errorf("%+v", p)
	}
}

func TestPlanBlocksWhatCannotBeWritten(t *testing.T) {
	devs := []Device{{ID: 1, CCID: "200U13", Ports: []Port{{"P1", 1}, {"P2", 1}, {"P3", 1}}}}
	a, b, c := pduInfo(1, "P1", 4, nil), pduInfo(2, "P2", 4, nil), pduInfo(3, "P3", 4, nil)
	a.HasWrite = false
	b.Namer = false
	p := Build(devs, []PDUInfo{a, b, c}, FormatCCID, now)
	if len(p.Changes) != 3 || !strings.Contains(p.Changes[0].Blocked, "write community") || !strings.Contains(p.Changes[1].Blocked, "cannot rename") || p.Changes[2].Blocked != "" {
		t.Errorf("%+v", p.Changes)
	}
}

func TestOutletNameFormats(t *testing.T) {
	d := Device{CCID: "200U13", Name: "a42sf"}
	for _, c := range []struct {
		d      Device
		format string
		want   string
		short  bool
	}{
		{d, FormatCCID, "200U13", false},
		{d, FormatCCIDName, "200U13 a42sf", false},
		{d, "", "200U13", false}, // unknown format: the id alone
		{Device{CCID: "200U13", Name: ""}, FormatCCIDName, "200U13", false},
		{Device{CCID: "200U13", Name: "200u13"}, FormatCCIDName, "200U13", false}, // the same words twice are pointless
		{Device{CCID: "200U13", Name: "very-long-host-name.example.com"}, FormatCCIDName, "200U13 very-long-hos", true},
		{Device{CCID: "N200U43", Name: "sw 1 ünï"}, FormatCCIDName, "N200U43 sw1n", false},             // only host name characters are kept
		{Device{CCID: "10FU12-123456789", Name: "abcdefgh"}, FormatCCIDName, "10FU12-123456789", true}, // no room for a useful part: the id alone
	} {
		got, short, err := OutletName(c.d, c.format)
		if err != nil || got != c.want || short != c.short {
			t.Errorf("%+v %s: %q %v %v, want %q %v", c.d, c.format, got, short, err, c.want, c.short)
		}
		if len(got) > 20 {
			t.Errorf("%q is longer than a PDU takes", got)
		}
	}
	for _, bad := range []Device{{CCID: strings.Repeat("9", 21)}, {CCID: "200U1\x00"}, {CCID: "ünï"}} {
		if _, _, err := OutletName(bad, FormatCCID); err == nil {
			t.Errorf("%q was accepted", bad.CCID)
		}
	}
}

func TestPlanInTheTwoFormatsAndTheTransition(t *testing.T) {
	devs := []Device{{ID: 1, CCID: "200U13", Name: "a42sf", Ports: []Port{{"P1", 1}, {"P2", 1}}}}
	pdus := []PDUInfo{pduInfo(1, "P1", 4, map[int]string{1: "200U13"}), pduInfo(2, "P2", 4, map[int]string{1: "200U13 a42sf"})}
	ids := Build(devs, pdus, FormatCCID, now)
	if ids.Unchanged != 1 || len(ids.Changes) != 1 || ids.Changes[0].PDU != "P2" || ids.Changes[0].Desired != "200U13" || ids.Changes[0].Kind != "rename" {
		t.Errorf("%+v", ids)
	}
	both := Build(devs, pdus, FormatCCIDName, now)
	if both.Unchanged != 1 || len(both.Changes) != 1 || both.Changes[0].PDU != "P1" || both.Changes[0].Desired != "200U13 a42sf" {
		t.Errorf("%+v", both)
	}
}
