package app_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kblaskoff/pdu-monitor/server/internal/app"
	"github.com/kblaskoff/pdu-monitor/server/internal/pdu"
	"github.com/kblaskoff/pdu-monitor/server/internal/store"
)

type env struct {
	app  *app.App
	st   *store.Store
	sims map[string]*pdu.Simulated
	rack store.Rack
	a, b store.PDU
}

// faulty wraps a simulated PDU and makes switching of chosen outlets fail
type faulty struct {
	*pdu.Simulated
	failOutlet map[int]bool
}

func (f faulty) SetOutlet(ctx context.Context, n int, on bool) error {
	if f.failOutlet[n] {
		return errors.New("timeout")
	}
	return f.Simulated.SetOutlet(ctx, n, on)
}

func setup(t *testing.T) *env {
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
	e := &env{st: st, sims: map[string]*pdu.Simulated{}}
	e.rack, _ = st.SaveRack(ctx, store.Rack{Name: "200", MaxAmps: 24})
	e.sims["a"] = pdu.NewSimulated(pdu.APC, "AP7932", 24, false, map[int]pdu.SimOutlet{1: {Name: "200U31", Amps: 1}})
	e.sims["b"] = pdu.NewSimulated(pdu.CyberPower, "PDU81007", 16, true, map[int]pdu.SimOutlet{1: {Name: "200U31", Amps: 1}, 2: {Name: "200U32", Amps: 3}})
	e.a, _ = st.SavePDU(ctx, store.PDU{RackID: e.rack.ID, Name: "P200A", Vendor: pdu.APC, Host: "a", Port: 161, MaxAmps: 24}, store.Communities{Read: "r", Write: "w"}, false)
	e.b, _ = st.SavePDU(ctx, store.PDU{RackID: e.rack.ID, Name: "P200B", Vendor: pdu.CyberPower, Host: "b", Port: 161, MaxAmps: 24}, store.Communities{Read: "r", Write: "w"}, false)
	e.app = app.New(st, nil, func(p store.PDU, c store.Communities) (pdu.Driver, error) {
		if s, ok := e.sims[p.Host]; ok {
			return s, nil
		}
		return nil, errors.New("unknown host " + p.Host)
	})
	e.app.Sleep = func(context.Context, time.Duration) error { return nil }
	return e
}

func waitDone(t *testing.T, a *app.App, id string) app.Operation {
	t.Helper()
	for i := 0; i < 500; i++ {
		if op, ok := a.Operation(id); ok && !op.Running {
			return op
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the operation did not finish")
	return app.Operation{}
}

func TestPollingKeepsReadingsAndSamplesOncePerMinute(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	now := time.Now()
	e.app.Now = func() time.Time { return now }
	e.app.PollPDUs(ctx)
	states, _, err := e.app.States(ctx)
	if err != nil || len(states) != 2 || states[0].Snapshot == nil || states[1].Snapshot == nil || states[0].Error != "" {
		t.Fatalf("%+v %v", states, err)
	}
	e.app.PollPDUs(ctx) // the same minute: no second sample
	now = now.Add(61 * time.Second)
	e.app.PollPDUs(ctx)
	samples, _ := e.st.Samples(ctx, e.a.ID, time.Now().Add(-time.Hour))
	if len(samples) != 2 {
		t.Fatalf("%d samples", len(samples))
	}
}

func TestAFailedPollKeepsTheLastGoodReadingAndShowsTheError(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.app.PollPDUs(ctx)
	if _, err := e.st.SavePDU(ctx, store.PDU{ID: e.a.ID, RackID: e.rack.ID, Name: "P200A", Vendor: pdu.APC, Host: "gone", Port: 161, MaxAmps: 24}, store.Communities{}, false); err != nil {
		t.Fatal(err)
	}
	e.app.PollPDUs(ctx)
	states, _, _ := e.app.States(ctx)
	if states[0].Error == "" || states[0].Snapshot == nil {
		t.Fatalf("%+v", states[0])
	}
	// a PDU without a community says so
	c, _ := e.st.SavePDU(ctx, store.PDU{RackID: e.rack.ID, Name: "P200C", Vendor: pdu.APC, Host: "a", Port: 161, MaxAmps: 24}, store.Communities{}, false)
	e.app.PollPDUs(ctx, c.ID)
	states, _, _ = e.app.States(ctx)
	var found bool
	for _, s := range states {
		if s.Name == "P200C" {
			found = true
			if !strings.Contains(s.Error, "community") {
				t.Fatalf("%q", s.Error)
			}
		}
	}
	if !found {
		t.Fatal("not listed")
	}
	// deleted PDUs disappear
	_ = e.st.DeletePDU(ctx, c.ID)
	e.app.PollPDUs(ctx)
	if states, _, _ = e.app.States(ctx); len(states) != 2 {
		t.Fatal("the deleted PDU is still listed")
	}
}

func TestRestartOfADualFeedServerAndTheAuditLog(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.app.PollPDUs(ctx)
	targets := []pdu.Target{{PDUID: e.a.ID, Outlet: 1}, {PDUID: e.b.ID, Outlet: 1}}
	op, err := e.app.StartPower(ctx, "anna", "restart", targets)
	if err != nil || !op.Running || !strings.Contains(op.Title, "200U31") {
		t.Fatalf("%+v %v", op, err)
	}
	done := waitDone(t, e.app, op.ID)
	if done.Error != "" || done.Detail != "Done" {
		t.Fatalf("%+v", done)
	}
	for _, sim := range []*pdu.Simulated{e.sims["a"], e.sims["b"]} {
		if s, _ := sim.OutletState(ctx, 1); s == nil || !*s {
			t.Fatal("both ports must be on again")
		}
	}
	time.Sleep(50 * time.Millisecond)
	log, _ := e.st.AuditLog(ctx, 10)
	if len(log) != 2 || log[1].Action != "power.restart" || log[1].User != "anna" || log[0].Action != "power.restart.done" || !log[0].OK ||
		!strings.Contains(log[1].Detail, "200U31 — P200A outlet 1") || !strings.Contains(log[1].Detail, "P200B outlet 1") {
		t.Fatalf("%+v", log)
	}
	if got := e.app.RecentOperations(); len(got) != 1 || got[0].ID != op.ID {
		t.Fatal("recent operations")
	}
}

func TestFailedSwitchOffRollsBackAndIsAudited(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.app.Factory = func(p store.PDU, c store.Communities) (pdu.Driver, error) {
		if p.Host == "b" {
			return faulty{e.sims["b"], map[int]bool{1: true}}, nil
		}
		return e.sims[p.Host], nil
	}
	e.app.PollPDUs(ctx)
	op, err := e.app.StartPower(ctx, "boris", "restart", []pdu.Target{{PDUID: e.a.ID, Outlet: 1}, {PDUID: e.b.ID, Outlet: 1}})
	if err != nil {
		t.Fatal(err)
	}
	done := waitDone(t, e.app, op.ID)
	if !strings.Contains(done.Error, "Could not switch off") || !strings.Contains(done.Error, "switched back on") {
		t.Fatalf("%q", done.Error)
	}
	if s, _ := e.sims["a"].OutletState(ctx, 1); s == nil || !*s {
		t.Fatal("the port that was switched off must be on again")
	}
	time.Sleep(50 * time.Millisecond)
	log, _ := e.st.AuditLog(ctx, 10)
	if log[0].Action != "power.restart.failed" || log[0].OK {
		t.Fatalf("%+v", log[0])
	}
}

func TestValidationAndBusyPorts(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.app.PollPDUs(ctx)
	one := []pdu.Target{{PDUID: e.b.ID, Outlet: 2}}
	for name, call := range map[string]func() error{
		"kind":  func() error { _, err := e.app.StartPower(ctx, "x", "format", one); return err },
		"empty": func() error { _, err := e.app.StartPower(ctx, "x", "off", nil); return err },
		"no pdu": func() error {
			_, err := e.app.StartPower(ctx, "x", "off", []pdu.Target{{PDUID: 999, Outlet: 1}})
			return err
		},
		"outlet 0": func() error {
			_, err := e.app.StartPower(ctx, "x", "off", []pdu.Target{{PDUID: e.a.ID, Outlet: 0}})
			return err
		},
		"outlet 99": func() error {
			_, err := e.app.StartPower(ctx, "x", "off", []pdu.Target{{PDUID: e.a.ID, Outlet: 97}})
			return err
		},
	} {
		if call() == nil {
			t.Errorf("%s accepted", name)
		}
	}
	// a restart that is waiting holds its ports: a second operation on them is refused
	release := make(chan struct{})
	var once sync.Once
	e.app.Sleep = func(ctx context.Context, d time.Duration) error { once.Do(func() { <-release }); return nil }
	op, err := e.app.StartPower(ctx, "anna", "restart", one)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if _, err := e.app.StartPower(ctx, "boris", "off", one); !errors.Is(err, app.ErrBusy) {
		t.Fatalf("%v", err)
	}
	close(release)
	waitDone(t, e.app, op.ID)
	if _, err := e.app.StartPower(ctx, "boris", "off", one); err != nil {
		t.Fatalf("free again: %v", err)
	}
}

func TestTestPDUUsesTheTypedValuesAndSavesNothing(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	snap, err := e.app.TestPDU(ctx, store.PDU{Vendor: pdu.APC, Host: "a", Port: 161}, store.Communities{Read: "x"})
	if err != nil || snap.Info.Outlets != 24 {
		t.Fatalf("%v %v", snap, err)
	}
	if _, err := e.app.TestPDU(ctx, store.PDU{Vendor: pdu.APC, Host: "a", Port: 161}, store.Communities{}); err == nil {
		t.Fatal("needs a community")
	}
	if _, err := e.app.TestPDU(ctx, store.PDU{Vendor: pdu.APC, Host: "nowhere", Port: 161}, store.Communities{Read: "x"}); err == nil {
		t.Fatal("unknown host")
	}
	if pdus, _ := e.st.PDUs(ctx); len(pdus) != 2 {
		t.Fatal("nothing may be saved")
	}
}
