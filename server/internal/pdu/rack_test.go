package pdu_test

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kblaskoff/pdu-monitor/server/internal/pdu"
)

func fp(v float64) *float64 { return &v }
func bp(v bool) *bool       { return &v }

func state(id int64, name string, limit float64, amps, watts *float64, outlets ...pdu.Outlet) pdu.State {
	snap := &pdu.Snapshot{Outlet: outlets, TotalAmps: amps, TotalWatts: watts}
	return pdu.State{ID: id, Name: name, Vendor: pdu.CyberPower, MaxAmps: limit, Snapshot: snap}
}

func TestOutletNamesAndKinds(t *testing.T) {
	for _, n := range []string{"", "  ", "Outlet_7", "Outlet 7", "outlet-07", "OUTLET7", "Outlet"} {
		if !pdu.IsDefaultOutletName(n) {
			t.Errorf("%q is a default name", n)
		}
	}
	for _, n := range []string{"200U31", "200U12-1", "N200U43", "Outlet_main", "a41sf"} {
		if pdu.IsDefaultOutletName(n) {
			t.Errorf("%q is not a default name", n)
		}
	}
	kinds := map[string]string{"N200U43": "network", "200U43": "server", "NAS01": "server", "n12u3": "network", "N10FU30": "network",
		"N20EU4": "network", "10FU30": "server", "NU5": "server", "N10F": "server"}
	for name, want := range kinds {
		if got := pdu.ServerKind(name); got != want {
			t.Errorf("%s: %s, want %s", name, got, want)
		}
	}
	if pdu.ServerKey(" 200u31 ") != "200u31" {
		t.Error("key")
	}
}

func TestTwoPDUsAreJoinedIntoOneServer(t *testing.T) {
	a := state(1, "P200A", 24, fp(3), nil,
		pdu.Outlet{Number: 1, Name: "200U31", On: bp(true), Amps: fp(0.3), Watts: fp(47)},
		pdu.Outlet{Number: 2, Name: "Outlet_2", On: bp(true), Amps: fp(0), Watts: fp(0)},
		pdu.Outlet{Number: 3, Name: "N200U43", On: bp(true), Amps: fp(0.3), Watts: fp(64)},
		pdu.Outlet{Number: 4, Name: "200U9", On: bp(true), Amps: fp(1), Watts: fp(100)})
	b := state(2, "P200B", 24, fp(1), nil,
		pdu.Outlet{Number: 1, Name: "200u31", On: bp(true), Amps: fp(0.2), Watts: fp(10)},
		pdu.Outlet{Number: 2, Name: "200U9", On: bp(false), Amps: fp(0), Watts: fp(0)})
	servers := pdu.Servers([]pdu.State{a, b})
	var names []string
	for _, s := range servers {
		names = append(names, s.Name)
	}
	if strings.Join(names, ",") != "200U9,200U31,N200U43" { // natural order: 9 before 31
		t.Fatal(names)
	}
	u31 := servers[1]
	if len(u31.Ports) != 2 || u31.Ports[0].PDUName != "P200A" || u31.Ports[1].PDUName != "P200B" {
		t.Fatalf("%+v", u31.Ports)
	}
	if w := u31.Watts(); w == nil || *w != 57 {
		t.Error("watts", w)
	}
	if a := u31.Amps(); a == nil || *a < 0.499 || *a > 0.501 {
		t.Error("amps", a)
	}
	if u31.Power() != "on" || servers[0].Power() != "mixed" || servers[2].Kind != "network" || len(u31.Targets()) != 2 {
		t.Error("power/kind")
	}
}

func TestUnmeteredServerHasNoWatts(t *testing.T) {
	s := pdu.Servers([]pdu.State{state(1, "P", 24, fp(5), nil, pdu.Outlet{Number: 1, Name: "S1", On: bp(true)})})
	if s[0].Watts() != nil || s[0].Amps() != nil {
		t.Error("no figures expected")
	}
}

func TestRackSummaryAndLimits(t *testing.T) {
	a := state(1, "A", 24, fp(12.8), fp(2329))
	b := state(2, "B", 24, fp(11.2), fp(2150))
	s := pdu.Summarize(24, []pdu.State{a, b}, 0.8)
	if s.Amps == nil || *s.Amps < 23.99 || *s.Amps > 24.01 || *s.Watts != 4479 || s.Status != pdu.StatusWarning { // 24.0 of 24: at the limit, not over
		t.Fatalf("%+v", s)
	}
	s = pdu.Summarize(24, []pdu.State{a, b, state(3, "C", 24, fp(0.5), nil)}, 0.8)
	if s.Status != pdu.StatusOver {
		t.Fatal(s.Status)
	}
	// a single PDU over its own limit is noticed even when the rack total is fine
	big := pdu.Summarize(48, []pdu.State{state(1, "O", 24, fp(25), nil), state(2, "F", 24, fp(5), nil)}, 0.8)
	if big.Status != pdu.StatusOK || big.WorstPDU != pdu.StatusOver || big.Attention != pdu.StatusOver {
		t.Fatalf("%+v", big)
	}
}

func TestNoReadingsMeansUnknownNotZero(t *testing.T) {
	offline := pdu.State{ID: 1, Name: "A", MaxAmps: 24, Error: "timeout"}
	s := pdu.Summarize(24, []pdu.State{offline}, 0.8)
	if s.Amps != nil || s.Status != pdu.StatusUnknown || s.Attention != pdu.StatusUnknown || s.Offline != 1 {
		t.Fatalf("%+v", s)
	}
}

func TestLimitStatusBoundaries(t *testing.T) {
	limit := 24.0
	cases := []struct {
		amps *float64
		want pdu.Status
	}{{fp(19.1), pdu.StatusOK}, {fp(19.2), pdu.StatusWarning}, {fp(24), pdu.StatusWarning}, {fp(24.1), pdu.StatusOver}, {nil, pdu.StatusUnknown}}
	for _, c := range cases {
		if got := pdu.Evaluate(c.amps, &limit, 0.8); got != c.want {
			t.Errorf("%v: %s, want %s", c.amps, got, c.want)
		}
	}
	zero := 0.0
	if pdu.Evaluate(fp(5), &zero, 0.8) != pdu.StatusUnknown {
		t.Error("no limit")
	}
}

func TestSimulatedPDUAndRestartThroughTheSequencer(t *testing.T) {
	sim := pdu.NewSimulated(pdu.CyberPower, "PDU81007", 16, true, map[int]pdu.SimOutlet{1: {Name: "S1", Amps: 2}})
	ctx := context.Background()
	snap, err := sim.Poll(ctx)
	if err != nil || snap.TotalAmps == nil || *snap.TotalAmps < 1.5 {
		t.Fatalf("%v %v", snap, err)
	}
	seq := &pdu.Sequencer{Controller: simController{sim}, Sleep: func(context.Context, time.Duration) error { return nil }}
	if err := seq.Run(ctx, pdu.Operation{Kind: "restart", Delay: 5 * time.Second}, []pdu.Target{{PDUID: 1, Outlet: 1}}, nil); err != nil {
		t.Fatal(err)
	}
	if s, _ := sim.OutletState(ctx, 1); s == nil || !*s {
		t.Fatal("should be on again")
	}
}

type simController struct{ s *pdu.Simulated }

func (c simController) Set(ctx context.Context, t pdu.Target, on bool) error {
	return c.s.SetOutlet(ctx, t.Outlet, on)
}
func (c simController) State(ctx context.Context, t pdu.Target) (*bool, error) {
	return c.s.OutletState(ctx, t.Outlet)
}

// ---- sequencer with a fake controller

type fakeController struct {
	mu      sync.Mutex
	states  map[pdu.Target]bool
	failSet map[pdu.Target]bool
	stuckOn map[pdu.Target]bool // ignores OFF
	blind   map[pdu.Target]bool // cannot report its state
	events  []string
}

func newFake(targets ...pdu.Target) *fakeController {
	f := &fakeController{states: map[pdu.Target]bool{}, failSet: map[pdu.Target]bool{}, stuckOn: map[pdu.Target]bool{}, blind: map[pdu.Target]bool{}}
	for _, t := range targets {
		f.states[t] = true
	}
	return f
}

func (f *fakeController) Set(ctx context.Context, t pdu.Target, on bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	word := "OFF"
	if on {
		word = "ON"
	}
	f.events = append(f.events, word+" "+string(rune('0'+t.Outlet%10)))
	if f.failSet[t] {
		return errors.New("timeout")
	}
	if !on && f.stuckOn[t] {
		return nil
	}
	f.states[t] = on
	return nil
}

func (f *fakeController) State(ctx context.Context, t pdu.Target) (*bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.blind[t] {
		return nil, nil
	}
	v := f.states[t]
	return &v, nil
}

var (
	ta = pdu.Target{PDUID: 1, Outlet: 1}
	tb = pdu.Target{PDUID: 2, Outlet: 6}
)

func sequencer(f *fakeController, waited *time.Duration) *pdu.Sequencer {
	return &pdu.Sequencer{Controller: f,
		Label: func(t pdu.Target) string { return "p" + string(rune('0'+t.Outlet)) },
		Sleep: func(_ context.Context, d time.Duration) error { *waited += d; return nil }, ConfirmTimeout: 3 * time.Second, ConfirmInterval: time.Second}
}

func TestRestartSwitchesBothPortsOffWaitsThenBothOn(t *testing.T) {
	f := newFake(ta, tb)
	var waited time.Duration
	var steps []string
	err := sequencer(f, &waited).Run(context.Background(), pdu.Operation{Kind: "restart", Delay: 7 * time.Second}, []pdu.Target{ta, tb},
		func(p pdu.Progress) { steps = append(steps, p.Step) })
	if err != nil {
		t.Fatal(err)
	}
	ev := append([]string(nil), f.events...)
	if len(ev) != 4 {
		t.Fatal(ev)
	}
	first, last := append([]string(nil), ev[:2]...), append([]string(nil), ev[2:]...)
	sort.Strings(first)
	sort.Strings(last)
	if first[0] != "OFF 1" || first[1] != "OFF 6" || last[0] != "ON 1" || last[1] != "ON 6" {
		t.Fatalf("order: %v", ev)
	}
	if waited != 7*time.Second {
		t.Errorf("waited %s", waited)
	}
	if !f.states[ta] || !f.states[tb] {
		t.Error("both must be on")
	}
	if steps[0] != "switching-off" || steps[len(steps)-1] != "confirmed-on" {
		t.Errorf("steps %v", steps)
	}
}

func TestFailedOffOnOnePDURestoresTheOther(t *testing.T) {
	f := newFake(ta, tb)
	f.failSet[tb] = true
	var waited time.Duration
	err := sequencer(f, &waited).Run(context.Background(), pdu.Operation{Kind: "restart", Delay: 5 * time.Second}, []pdu.Target{ta, tb}, nil)
	var se *pdu.SequenceError
	if !errors.As(err, &se) || se.Phase != "off" || !se.Restored || !strings.Contains(se.Detail, "p6") {
		t.Fatalf("%v", err)
	}
	if !f.states[ta] {
		t.Error("the port that was switched off must be on again")
	}
	for _, e := range f.events {
		if e == "ON 6" && !f.failSet[tb] {
			t.Error("the failed port was never switched off")
		}
	}
}

func TestOffThatThePDUDoesNotConfirmIsRolledBack(t *testing.T) {
	f := newFake(ta, tb)
	f.stuckOn[tb] = true
	var waited time.Duration
	err := sequencer(f, &waited).Run(context.Background(), pdu.Operation{Kind: "restart", Delay: 5 * time.Second}, []pdu.Target{ta, tb}, nil)
	var se *pdu.SequenceError
	if !errors.As(err, &se) || se.Phase != "off-unconfirmed" || se.Detail != "p6" {
		t.Fatalf("%v", err)
	}
	if !f.states[ta] {
		t.Error("a must be on again")
	}
}

func TestPortThatCannotReportItsStateDoesNotBlockTheRestart(t *testing.T) {
	f := newFake(ta, tb)
	f.blind[tb] = true
	var waited time.Duration
	if err := sequencer(f, &waited).Run(context.Background(), pdu.Operation{Kind: "restart", Delay: 2 * time.Second}, []pdu.Target{ta, tb}, nil); err != nil {
		t.Fatal(err)
	}
	if !f.states[tb] {
		t.Error("must be on")
	}
}

func TestPlainOnAndOffAndFailedSwitchOn(t *testing.T) {
	f := newFake(ta)
	var waited time.Duration
	s := sequencer(f, &waited)
	if err := s.Run(context.Background(), pdu.Operation{Kind: "off"}, []pdu.Target{ta}, nil); err != nil || f.states[ta] {
		t.Fatal(err, f.states[ta])
	}
	if err := s.Run(context.Background(), pdu.Operation{Kind: "on"}, []pdu.Target{ta}, nil); err != nil || !f.states[ta] {
		t.Fatal(err)
	}
	f.failSet[ta] = true
	err := s.Run(context.Background(), pdu.Operation{Kind: "on"}, []pdu.Target{ta}, nil)
	var se *pdu.SequenceError
	if !errors.As(err, &se) || se.Phase != "on" {
		t.Fatal(err)
	}
	if err := s.Run(context.Background(), pdu.Operation{Kind: "format"}, []pdu.Target{ta}, nil); err == nil {
		t.Fatal("unknown operation")
	}
}

func TestCancelledDuringThePauseSwitchesThePortsBackOn(t *testing.T) {
	f := newFake(ta, tb)
	ctx, cancel := context.WithCancel(context.Background())
	s := &pdu.Sequencer{Controller: f, Sleep: func(ctx context.Context, d time.Duration) error { cancel(); return ctx.Err() }}
	err := s.Run(ctx, pdu.Operation{Kind: "restart", Delay: 5 * time.Second}, []pdu.Target{ta, tb}, nil)
	if err == nil {
		t.Fatal("expected cancellation")
	}
	if !f.states[ta] || !f.states[tb] {
		t.Error("a server must not be left without power")
	}
}
