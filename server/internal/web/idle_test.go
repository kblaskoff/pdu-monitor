package web_test

import (
	"context"
	"testing"
	"time"

	"github.com/kblaskoff/pdu-monitor/server/internal/store"
)

func TestHostIdleBars(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	hosts, _ := e.st.Hosts(ctx)
	var id int
	for _, h := range hosts {
		if h.Name == "a52sf" {
			id = int(h.ID)
		}
	}
	var rows []store.IdleDelta // a year of this host, an hour at a time
	for h := time.Now().Add(-400 * 24 * time.Hour).Truncate(time.Hour); h.Before(time.Now()); h = h.Add(time.Hour) {
		rows = append(rows, store.IdleDelta{HostID: int64(id), Hour: h.Unix() / 3600, Sum: 70 * 60, N: 60, Min: 50})
	}
	if err := e.st.AddIdle(ctx, rows); err != nil {
		t.Fatal(err)
	}
	vera := e.login("vera")
	status, out := vera.call("GET", "/api/hosts/"+itoa(id)+"/idle", nil)
	if status != 200 || out["unit"] != "day" || len(list(out["buckets"])) != 14 {
		t.Fatalf("%d %v", status, out)
	}
	b := obj(list(out["buckets"])[0])
	if b["avg"] == nil || b["min"] == nil || b["n"].(float64) < 1400 || b["t"] == nil {
		t.Errorf("a day with readings: %v", b)
	}
	if out["firstData"] == nil {
		t.Error("firstData")
	}
	status, out = vera.call("GET", "/api/hosts/"+itoa(id)+"/idle?range=1y&tz=-120", nil)
	if status != 200 || out["unit"] != "week" || len(list(out["buckets"])) != 53 {
		t.Fatalf("%d %v", status, out)
	}
	if n := obj(list(out["buckets"])[1])["n"].(float64); n < 9000 { // a whole week: 7 * 24 * 60 = 10080 readings
		t.Errorf("a week: %v", n)
	}
	// the days are those of the viewer: the same host, another time zone, other day limits
	_, utc := vera.call("GET", "/api/hosts/"+itoa(id)+"/idle?tz=0", nil)
	_, east := vera.call("GET", "/api/hosts/"+itoa(id)+"/idle?tz=-600", nil)
	if obj(list(utc["buckets"])[13])["t"] == obj(list(east["buckets"])[13])["t"] {
		t.Error("the time zone is not used")
	}
	// bad requests; a host that does not exist; and nobody who is not logged in
	for _, q := range []string{"?range=5y", "?tz=abc", "?tz=2000", "?range=14d%3Bx"} {
		if status, _ := vera.call("GET", "/api/hosts/"+itoa(id)+"/idle"+q, nil); status != 400 {
			t.Errorf("%s: %d", q, status)
		}
	}
	if status, _ := vera.call("GET", "/api/hosts/99999/idle", nil); status != 404 {
		t.Errorf("unknown host: %d", status)
	}
	if status, _ := e.anonymous().call("GET", "/api/hosts/"+itoa(id)+"/idle", nil); status != 401 {
		t.Errorf("anonymous: %d", status)
	}
}

func TestHostWithoutHistoryHasEmptyBars(t *testing.T) {
	e := newEnv(t)
	hosts, _ := e.st.Hosts(context.Background())
	vera := e.login("vera")
	_, out := vera.call("GET", "/api/hosts/"+itoa(int(hosts[0].ID))+"/idle", nil)
	bs := list(out["buckets"])
	if len(bs) != 14 {
		t.Fatalf("%v", out)
	}
	for _, b := range bs[:13] {
		if obj(b)["avg"] != nil || obj(b)["n"] != float64(0) {
			t.Fatalf("a day without readings has no numbers: %v", b)
		}
	}
	if out["firstData"] != nil && obj(bs[13])["n"].(float64) == 0 {
		t.Errorf("%v", out)
	}
}

// An agent samples once a minute: a reading of 100 s ago is current even when this server reads every 15 s.
func TestAgentReadingIsNotOldJustBecauseTheServerReadsOften(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	st, _ := e.st.Settings(ctx)
	st.HostPollSeconds = 15
	if _, err := e.st.SaveSettings(ctx, st); err != nil {
		t.Fatal(err)
	}
	vera := e.login("vera")
	online := func(after time.Duration) bool {
		e.srv.Now = func() time.Time { return time.Now().Add(after) }
		_, out := vera.call("GET", "/api/hosts", nil)
		for _, h := range list(out["hosts"]) {
			if obj(h)["name"] == "a52sf" {
				return obj(h)["online"] == true
			}
		}
		t.Fatal("no a52sf")
		return false
	}
	if !online(100 * time.Second) {
		t.Error("100 s old is current for an agent that samples every 60 s")
	}
	if online(400 * time.Second) {
		t.Error("400 s old is old")
	}
}
