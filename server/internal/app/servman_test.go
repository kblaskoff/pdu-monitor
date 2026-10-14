package app_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kblaskoff/pdu-monitor/server/internal/app"
	"github.com/kblaskoff/pdu-monitor/server/internal/pdu"
	"github.com/kblaskoff/pdu-monitor/server/internal/servman"
	"github.com/kblaskoff/pdu-monitor/server/internal/store"
)

type fakeServman struct {
	mu      sync.Mutex
	devices []servman.Device
	err     error
	calls   int
}

func (f *fakeServman) Devices(context.Context) ([]servman.Device, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return append([]servman.Device(nil), f.devices...), f.err
}

func (f *fakeServman) Calls() int { f.mu.Lock(); defer f.mu.Unlock(); return f.calls }

func smDevices() []servman.Device {
	return []servman.Device{
		{ID: 1, CCID: "200U31", Name: "h35sf", Ports: []servman.Port{{PDU: "P200A", Port: 1}, {PDU: "P200B", Port: 1}}}, // named already
		{ID: 2, CCID: "200U13", Name: "a42sf", Ports: []servman.Port{{PDU: "P200A", Port: 14}, {PDU: "P200B", Port: 14}}},
		{ID: 3, CCID: "200U32", Name: "x", Ports: []servman.Port{{PDU: "P200B", Port: 2}}}, // named already
	}
}

func names(t *testing.T, e *env, id int64) map[int]string {
	t.Helper()
	states, _, _ := e.app.States(context.Background())
	out := map[int]string{}
	for _, s := range states {
		if s.ID == id && s.Snapshot != nil {
			for _, o := range s.Snapshot.Outlet {
				out[o.Number] = o.Name
			}
		}
	}
	return out
}

func setupServman(t *testing.T) (*env, *fakeServman) {
	e := setup(t)
	f := &fakeServman{devices: smDevices()}
	e.app.Servman = f
	e.app.PollPDUs(context.Background())
	return e, f
}

func auditActions(t *testing.T, e *env, action string) []store.AuditEntry {
	log, _ := e.st.AuditLog(context.Background(), 100)
	var out []store.AuditEntry
	for _, l := range log {
		if l.Action == action {
			out = append(out, l)
		}
	}
	return out
}

func TestServmanPlanWritesNothing(t *testing.T) {
	e, _ := setupServman(t)
	plan, err := e.app.ServmanPlan(context.Background())
	if err != nil || len(plan.Changes) != 2 || plan.Unchanged != 3 {
		t.Fatalf("%v %+v", err, plan)
	}
	if names(t, e, e.a.ID)[14] == "200U13" || names(t, e, e.b.ID)[14] == "200U13" {
		t.Fatal("a plan changed a PDU")
	}
	st := e.app.ServmanStatus()
	if !st.Configured || st.Plan == nil || st.PlanAt == nil || st.PlanError != "" {
		t.Errorf("%+v", st)
	}
	if len(auditActions(t, e, "outlet.names")) != 0 {
		t.Error("the log has a writing that did not happen")
	}
}

func TestServmanNotConfiguredAndErrors(t *testing.T) {
	e := setup(t)
	if _, err := e.app.ServmanPlan(context.Background()); !errors.Is(err, app.ErrNoServman) {
		t.Errorf("%v", err)
	}
	if _, err := e.app.ApplyNames(context.Background(), "anna", app.NameSelection{All: true}, false); !errors.Is(err, app.ErrNoServman) {
		t.Errorf("%v", err)
	}
	if e.app.ServmanStatus().Configured {
		t.Error("configured")
	}
	f := &fakeServman{err: errors.New("down")}
	e.app.Servman = f
	if _, err := e.app.ApplyNames(context.Background(), "anna", app.NameSelection{All: true}, false); err == nil || !strings.Contains(err.Error(), "down") {
		t.Errorf("servman is down: %v", err)
	}
	if st := e.app.ServmanStatus(); st.PlanError == "" {
		t.Error("the error is not shown")
	}
	if _, err := e.app.ApplyNames(context.Background(), "anna", app.NameSelection{}, false); err == nil {
		t.Error("nothing chosen")
	}
}

func TestApplyWritesVerifiesAndLogsEveryPDU(t *testing.T) {
	e, _ := setupServman(t)
	ctx := context.Background()
	res, err := e.app.ApplyNames(ctx, "anna", app.NameSelection{All: true}, false)
	if err != nil || res.Written != 2 || res.Verified != 2 || len(res.Failed) != 0 || res.Skipped != 0 {
		t.Fatalf("%v %+v", err, res)
	}
	if names(t, e, e.a.ID)[14] != "200U13" || names(t, e, e.b.ID)[14] != "200U13" {
		t.Fatalf("%v %v", names(t, e, e.a.ID), names(t, e, e.b.ID))
	}
	log := auditActions(t, e, "outlet.names")
	if len(log) != 2 || !log[0].OK || log[0].User != "anna" || !strings.Contains(log[0].Detail+log[1].Detail, `#14 "Outlet_14"→"200U13"`) {
		t.Errorf("%+v", log)
	}
	// what is left is nothing
	if plan := e.app.ServmanStatus().Plan; plan == nil || len(plan.Changes) != 0 || plan.Unchanged != 5 {
		t.Errorf("%+v", plan)
	}
	again, err := e.app.ApplyNames(ctx, "anna", app.NameSelection{All: true}, false)
	if err != nil || again.Written != 0 {
		t.Errorf("a second writing: %v %+v", err, again)
	}
}

func TestApplyCanBeLimitedToOnePDUOrOneOutlet(t *testing.T) {
	e, _ := setupServman(t)
	ctx := context.Background()
	res, err := e.app.ApplyNames(ctx, "anna", app.NameSelection{PDU: "p200b", Outlet: 14}, false)
	if err != nil || res.Written != 1 || res.Verified != 1 {
		t.Fatalf("%v %+v", err, res)
	}
	if names(t, e, e.a.ID)[14] == "200U13" {
		t.Error("another PDU was written")
	}
	res, _ = e.app.ApplyNames(ctx, "anna", app.NameSelection{PDU: "P200A"}, false)
	if res.Written != 1 || names(t, e, e.a.ID)[14] != "200U13" {
		t.Errorf("%+v", res)
	}
	if res, _ := e.app.ApplyNames(ctx, "anna", app.NameSelection{PDU: "P404"}, false); res.Written != 0 {
		t.Errorf("a PDU that does not exist: %+v", res)
	}
}

func TestPDUWithoutWriteCommunityIsNotWrittenTo(t *testing.T) {
	e, _ := setupServman(t)
	ctx := context.Background()
	if _, err := e.st.SavePDU(ctx, e.b, store.Communities{Read: "r"}, true); err != nil { // the write community is removed
		t.Fatal(err)
	}
	e.app.PollPDUs(ctx)
	res, err := e.app.ApplyNames(ctx, "anna", app.NameSelection{All: true}, false)
	if err != nil || res.Written != 1 || res.Skipped != 1 {
		t.Fatalf("%v %+v", err, res)
	}
	if names(t, e, e.b.ID)[14] == "200U13" {
		t.Error("written without a write community")
	}
	plan := e.app.ServmanStatus().Plan
	if len(plan.Changes) != 1 || !strings.Contains(plan.Changes[0].Blocked, "write community") {
		t.Errorf("%+v", plan.Changes)
	}
}

// namerFault makes writing names fail or lie
type namerFault struct {
	*pdu.Simulated
	fail map[int]bool
	lie  bool
	hold chan struct{}
}

func (f namerFault) SetOutletName(ctx context.Context, n int, name string) error {
	if f.hold != nil {
		<-f.hold
	}
	if f.fail[n] {
		return errors.New("timeout")
	}
	if f.lie {
		return nil // "ok", and nothing changes
	}
	return f.Simulated.SetOutletName(ctx, n, name)
}

func faultyEnv(t *testing.T, mk func(*pdu.Simulated) pdu.Driver) (*env, *fakeServman) {
	e := setup(t)
	f := &fakeServman{devices: smDevices()}
	e.app.Servman = f
	e.app = app.New(e.st, nil, func(p store.PDU, c store.Communities) (pdu.Driver, error) {
		return mk(e.sims[p.Host]), nil
	})
	e.app.Servman = f
	e.app.Sleep = func(context.Context, time.Duration) error { return nil }
	e.app.PollPDUs(context.Background())
	return e, f
}

func TestFailuresAreReportedAndTheLogSaysSo(t *testing.T) {
	e, _ := faultyEnv(t, func(s *pdu.Simulated) pdu.Driver { return namerFault{Simulated: s, fail: map[int]bool{14: true}} })
	res, err := e.app.ApplyNames(context.Background(), "anna", app.NameSelection{All: true}, false)
	if err != nil || res.Written != 0 || len(res.Failed) != 2 || !strings.Contains(res.Failed[0].Error, "timeout") {
		t.Fatalf("%v %+v", err, res)
	}
	for _, l := range auditActions(t, e, "outlet.names") {
		if l.OK {
			t.Errorf("a failed writing is logged as ok: %+v", l)
		}
	}
}

func TestAPDUThatAnswersOkButKeepsTheOldNameIsCaught(t *testing.T) {
	e, _ := faultyEnv(t, func(s *pdu.Simulated) pdu.Driver { return namerFault{Simulated: s, lie: true} })
	res, err := e.app.ApplyNames(context.Background(), "anna", app.NameSelection{All: true}, false)
	if err != nil || res.Written != 2 || res.Verified != 0 || len(res.Failed) != 2 || !strings.Contains(res.Failed[0].Error, "accepted the name but shows") {
		t.Fatalf("%v %+v", err, res)
	}
}

func TestAPDUThatKeepsFailingIsGivenUp(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	var devs []servman.Device
	for i := 1; i <= 8; i++ {
		devs = append(devs, servman.Device{ID: int64(i), CCID: fmt.Sprintf("200U%d", 40+i), Ports: []servman.Port{{PDU: "P200A", Port: 2 + i}}})
	}
	e.app.Servman = &fakeServman{devices: devs}
	e.app = app.New(e.st, nil, func(p store.PDU, c store.Communities) (pdu.Driver, error) {
		all := map[int]bool{}
		for i := 1; i <= 24; i++ {
			all[i] = true
		}
		return namerFault{Simulated: e.sims[p.Host], fail: all}, nil
	})
	e.app.Servman = &fakeServman{devices: devs}
	e.app.PollPDUs(ctx)
	res, _ := e.app.ApplyNames(ctx, "anna", app.NameSelection{All: true}, false)
	if len(res.Failed) != 8 || !strings.Contains(res.Failed[7].Error, "repeated errors") || strings.Contains(res.Failed[2].Error, "repeated") {
		t.Errorf("%+v", res.Failed)
	}
}

func TestOnlyOneWritingAtATime(t *testing.T) {
	hold := make(chan struct{})
	e, _ := faultyEnv(t, func(s *pdu.Simulated) pdu.Driver { return namerFault{Simulated: s, hold: hold} })
	ctx := context.Background()
	done := make(chan error, 1)
	go func() {
		_, err := e.app.ApplyNames(ctx, "anna", app.NameSelection{All: true}, false)
		done <- err
	}()
	for i := 0; i < 200 && !e.app.ServmanStatus().Writing; i++ {
		time.Sleep(5 * time.Millisecond)
	}
	if !e.app.ServmanStatus().Writing {
		t.Fatal("not writing")
	}
	if _, err := e.app.ApplyNames(ctx, "boris", app.NameSelection{All: true}, false); !errors.Is(err, app.ErrNamesBusy) {
		t.Errorf("a second writing: %v", err)
	}
	close(hold)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if e.app.ServmanStatus().Writing {
		t.Error("still writing")
	}
}

func TestTheAutomaticSyncWritesALimitedNumberPerRound(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	var devs []servman.Device
	for i := 1; i <= 24; i++ { // 24 outlets of P200A
		devs = append(devs, servman.Device{ID: int64(i), CCID: fmt.Sprintf("200U%d", 100+i), Ports: []servman.Port{{PDU: "P200A", Port: i}}})
	}
	// four more PDUs of 24 outlets: 120 changes in all
	for k := 0; k < 4; k++ {
		host := fmt.Sprintf("x%d", k)
		e.sims[host] = pdu.NewSimulated(pdu.APC, "AP7932", 24, false, nil)
		if _, err := e.st.SavePDU(ctx, store.PDU{RackID: e.rack.ID, Name: fmt.Sprintf("PX%d", k), Vendor: pdu.APC, Host: host, Port: 161, MaxAmps: 24}, store.Communities{Read: "r", Write: "w"}, false); err != nil {
			t.Fatal(err)
		}
		for i := 1; i <= 24; i++ {
			devs = append(devs, servman.Device{ID: int64(1000 + k*100 + i), CCID: fmt.Sprintf("1%dU%d", k, i), Ports: []servman.Port{{PDU: fmt.Sprintf("PX%d", k), Port: i}}})
		}
	}
	e.app.Servman = &fakeServman{devices: devs}
	e.app.PollPDUs(ctx)
	res, err := e.app.ApplyNames(ctx, "servman-sync", app.NameSelection{All: true}, true)
	if err != nil || res.Written != 100 || res.Skipped != 20 {
		t.Fatalf("%v %+v", err, res)
	}
	res, _ = e.app.ApplyNames(ctx, "servman-sync", app.NameSelection{All: true}, true) // the next round writes the rest
	if res.Written != 20 || res.Skipped != 0 {
		t.Errorf("%+v", res)
	}
}

func TestTheLoopFollowsTheIntervalsOfTheSettings(t *testing.T) {
	e, f := setupServman(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// off: nothing is fetched
	if _, err := e.st.SaveSettings(ctx, store.Settings{ServmanPollMinutes: 0, ServmanSyncMinutes: 0, PollSeconds: 10, HostPollSeconds: 60, WarnPercent: 80, RestartDelaySecond: 8, SampleRetentionDay: 90}); err != nil {
		t.Fatal(err)
	}
	e.app.ServmanSweep = 15 * time.Millisecond
	go e.app.Run(ctx)
	time.Sleep(150 * time.Millisecond)
	if f.Calls() != 0 {
		t.Fatalf("servman was asked %d times although both intervals are off", f.Calls())
	}
	// polling on, writing off: a plan is made, nothing is written
	st, _ := e.st.Settings(ctx)
	st.ServmanPollMinutes = 60
	_, _ = e.st.SaveSettings(ctx, st)
	waitFor(t, func() bool { return e.app.ServmanStatus().Plan != nil })
	calls := f.Calls()
	time.Sleep(100 * time.Millisecond)
	if f.Calls() != calls {
		t.Errorf("asked again before the interval: %d -> %d", calls, f.Calls())
	}
	if names(t, e, e.a.ID)[14] == "200U13" {
		t.Fatal("written although the sync interval is off")
	}
	// writing on
	st.ServmanSyncMinutes = 30
	_, _ = e.st.SaveSettings(ctx, st)
	waitFor(t, func() bool { return names(t, e, e.a.ID)[14] == "200U13" && names(t, e, e.b.ID)[14] == "200U13" })
	if log := auditActions(t, e, "outlet.names"); len(log) == 0 || log[0].User != "servman-sync" {
		t.Errorf("%+v", log)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 400; i++ {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("it did not happen")
}
