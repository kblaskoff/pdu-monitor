// Package app ties the parts together: it polls the PDUs, keeps the last readings, runs the power operations (with the audit log) and
// owns the host poller.
package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kblaskoff/pdu-monitor/server/internal/hostinfo"
	"github.com/kblaskoff/pdu-monitor/server/internal/pdu"
	"github.com/kblaskoff/pdu-monitor/server/internal/servman"
	"github.com/kblaskoff/pdu-monitor/server/internal/snmp"
	"github.com/kblaskoff/pdu-monitor/server/internal/store"
)

// DriverFactory makes the driver of a PDU from its configuration and communities.
type DriverFactory func(p store.PDU, c store.Communities) (pdu.Driver, error)

// SNMPDriver is the real factory: SNMPv1 over UDP. debug, when not nil, gets one line per request.
func SNMPDriver(debug func(string)) DriverFactory {
	return func(p store.PDU, c store.Communities) (pdu.Driver, error) {
		client := &snmp.UDPClient{Addr: net.JoinHostPort(p.Host, strconv.Itoa(p.Port)), ReadCommunity: c.Read, WriteCommunity: c.Write,
			Timeout: 2 * time.Second, Retries: 1, Debug: debug}
		return pdu.NewDriver(p.Vendor, client)
	}
}

// Reading is the last answer of a PDU.
type Reading struct {
	Snapshot *pdu.Snapshot
	Error    string
	At       time.Time
}

// ErrBusy: another operation is working on one of the ports.
var ErrBusy = errors.New("another operation is already switching one of these ports")

// Operation is a power operation that is running or has finished (the screen follows it).
type Operation struct {
	ID       string       `json:"id"`
	User     string       `json:"user"`
	Kind     string       `json:"kind"`
	Title    string       `json:"title"`
	Detail   string       `json:"detail"`
	Running  bool         `json:"running"`
	Error    string       `json:"error,omitempty"`
	Started  time.Time    `json:"started"`
	Finished *time.Time   `json:"finished,omitempty"`
	Targets  []pdu.Target `json:"targets"`
}

// App is the running application.
type App struct {
	Store   *store.Store
	Hosts   *hostinfo.Poller
	Factory DriverFactory
	Log     func(string)
	Now     func() time.Time
	// Sleep is used in the pause of a restart (replaced in tests).
	Sleep func(ctx context.Context, d time.Duration) error

	// Servman tells which device is plugged into which outlet; nil when it is not configured.
	Servman servman.Source
	// ServmanSweep is how often the settings are looked at to see whether servman is due (30 s; shorter in tests).
	ServmanSweep time.Duration

	snMu         sync.Mutex // the plan and the state of the writing of names
	lastPlan     *servman.Plan
	planAt       *time.Time
	planErr      string
	syncAt       *time.Time
	syncRes      string
	writing      bool
	lastApply    *NameResult
	lastApplyErr string

	idle *idleKeeper

	mu       sync.Mutex
	readings map[int64]Reading
	drivers  map[int64]*cachedDriver
	lastSave map[int64]time.Time
	ops      map[string]*Operation
	opOrder  []string
	busy     map[pdu.Target]string
}

type cachedDriver struct {
	signature string
	driver    pdu.Driver
}

func New(s *store.Store, hosts *hostinfo.Poller, factory DriverFactory) *App {
	a := &App{Store: s, Hosts: hosts, Factory: factory, Now: time.Now, readings: map[int64]Reading{}, drivers: map[int64]*cachedDriver{},
		lastSave: map[int64]time.Time{}, ops: map[string]*Operation{}, busy: map[pdu.Target]string{}, idle: newIdleKeeper()}
	if hosts != nil {
		hosts.OnStatus = a.recordIdle
	}
	return a
}

func (a *App) logf(format string, args ...any) {
	if a.Log != nil {
		a.Log(fmt.Sprintf(format, args...))
	}
}

// driver returns the driver of a PDU; it is made again when the configuration or a community changed.
func (a *App) driver(ctx context.Context, p store.PDU) (pdu.Driver, error) {
	c, err := a.Store.PDUCommunities(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	if c.Read == "" {
		return nil, errors.New("this PDU has no read community: edit it and enter one")
	}
	sig := fmt.Sprintf("%s|%s|%d|%s|%s", p.Vendor, p.Host, p.Port, c.Read, c.Write)
	a.mu.Lock()
	defer a.mu.Unlock()
	if d := a.drivers[p.ID]; d != nil && d.signature == sig {
		return d.driver, nil
	}
	drv, err := a.Factory(p, c)
	if err != nil {
		return nil, err
	}
	a.drivers[p.ID] = &cachedDriver{signature: sig, driver: drv}
	return drv, nil
}

// PollPDUs reads all the PDUs (or only the given ids) at the same time.
func (a *App) PollPDUs(ctx context.Context, only ...int64) {
	pdus, err := a.Store.PDUs(ctx)
	if err != nil {
		a.logf("pdus: %v", err)
		return
	}
	want := map[int64]bool{}
	for _, id := range only {
		want[id] = true
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, 12)
	var samples []store.Sample
	var smu sync.Mutex
	known := map[int64]bool{}
	for _, p := range pdus {
		known[p.ID] = true
		if len(want) > 0 && !want[p.ID] {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(p store.PDU) {
			defer wg.Done()
			defer func() { <-sem }()
			r := a.pollOne(ctx, p)
			a.mu.Lock()
			old := a.readings[p.ID]
			if r.Snapshot == nil && old.Snapshot != nil {
				r.Snapshot = old.Snapshot // the last good reading stays (the screen shows the error and dims it)
			}
			a.readings[p.ID] = r
			due := r.Error == "" && r.Snapshot != nil && a.Now().Sub(a.lastSave[p.ID]) >= 55*time.Second
			if due {
				a.lastSave[p.ID] = a.Now()
			}
			a.mu.Unlock()
			if due {
				smu.Lock()
				samples = append(samples, store.Sample{PDUID: p.ID, Time: r.At, Amps: r.Snapshot.TotalAmps, Watts: r.Snapshot.TotalWatts})
				smu.Unlock()
			}
		}(p)
	}
	wg.Wait()
	if len(samples) > 0 {
		if err := a.Store.AddSamples(ctx, samples); err != nil {
			a.logf("samples: %v", err)
		}
	}
	a.mu.Lock()
	for id := range a.readings { // deleted PDUs
		if !known[id] {
			delete(a.readings, id)
			delete(a.drivers, id)
			delete(a.lastSave, id)
		}
	}
	a.mu.Unlock()
}

func (a *App) pollOne(ctx context.Context, p store.PDU) Reading {
	drv, err := a.driver(ctx, p)
	if err != nil {
		return Reading{Error: err.Error(), At: a.Now()}
	}
	cctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	snap, err := drv.Poll(cctx)
	if err != nil {
		a.logf("PDU %s (%s:%d): %v", p.Name, p.Host, p.Port, err)
		return Reading{Error: err.Error(), At: a.Now()}
	}
	return Reading{Snapshot: snap, At: a.Now()}
}

// Run polls the PDUs, the hosts and prunes the history until the context ends.
func (a *App) Run(ctx context.Context) {
	go a.idleLoop(ctx)
	if a.Servman != nil {
		go a.servmanLoop(ctx)
	}
	if a.Hosts != nil {
		go a.Hosts.Run(ctx, func() time.Duration {
			st, _ := a.Store.Settings(ctx)
			return time.Duration(st.HostPollSeconds) * time.Second
		})
	}
	prune := time.NewTicker(6 * time.Hour)
	defer prune.Stop()
	a.pruneHistory(ctx)
	for {
		a.PollPDUs(ctx)
		st, _ := a.Store.Settings(ctx)
		select {
		case <-ctx.Done():
			return
		case <-prune.C:
			a.pruneHistory(ctx)
		case <-time.After(time.Duration(st.PollSeconds) * time.Second):
		}
	}
}

func (a *App) pruneHistory(ctx context.Context) {
	if _, err := a.Store.PruneIdle(ctx, a.Now().Add(-idleKeepDays*24*time.Hour)); err != nil {
		a.logf("prune idle: %v", err)
	}
	st, _ := a.Store.Settings(ctx)
	if n, err := a.Store.PruneSamples(ctx, a.Now().Add(-time.Duration(st.SampleRetentionDay)*24*time.Hour)); err != nil {
		a.logf("prune: %v", err)
	} else if n > 0 {
		a.logf("removed %d old samples", n)
	}
}

// States are the PDUs with their last readings.
func (a *App) States(ctx context.Context) ([]pdu.State, map[int64]store.PDU, error) {
	pdus, err := a.Store.PDUs(ctx)
	if err != nil {
		return nil, nil, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	configs := map[int64]store.PDU{}
	out := make([]pdu.State, 0, len(pdus))
	for _, p := range pdus {
		configs[p.ID] = p
		r := a.readings[p.ID]
		out = append(out, pdu.State{ID: p.ID, Name: p.Name, Vendor: p.Vendor, MaxAmps: p.MaxAmps, BankMaxAmps: p.BankMaxAmps, Snapshot: r.Snapshot, Error: r.Error})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, configs, nil
}

// ---- power operations

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

type controller struct{ a *App }

func (c controller) driverOf(ctx context.Context, id int64) (pdu.Driver, error) {
	p, err := c.a.Store.PDU(ctx, id)
	if err != nil {
		return nil, err
	}
	return c.a.driver(ctx, p)
}

func (c controller) Set(ctx context.Context, t pdu.Target, on bool) error {
	d, err := c.driverOf(ctx, t.PDUID)
	if err != nil {
		return err
	}
	return d.SetOutlet(ctx, t.Outlet, on)
}

func (c controller) State(ctx context.Context, t pdu.Target) (*bool, error) {
	d, err := c.driverOf(ctx, t.PDUID)
	if err != nil {
		return nil, err
	}
	return d.OutletState(ctx, t.Outlet)
}

// describe gives "200U31 — P200A outlet 1" for the audit log and the screen.
func (a *App) describe(ctx context.Context, t pdu.Target) string {
	name := fmt.Sprintf("PDU %d", t.PDUID)
	if p, err := a.Store.PDU(ctx, t.PDUID); err == nil {
		name = p.Name
	}
	a.mu.Lock()
	r := a.readings[t.PDUID]
	a.mu.Unlock()
	if r.Snapshot != nil {
		for _, o := range r.Snapshot.Outlet {
			if o.Number == t.Outlet && o.Assigned() {
				return fmt.Sprintf("%s — %s outlet %d", o.Name, name, t.Outlet)
			}
		}
	}
	return fmt.Sprintf("%s outlet %d", name, t.Outlet)
}

// StartPower starts an operation ("on", "off" or "restart") on ports and returns at once; the operation can be followed with Operation.
func (a *App) StartPower(ctx context.Context, user, kind string, targets []pdu.Target) (*Operation, error) {
	if kind != "on" && kind != "off" && kind != "restart" {
		return nil, errors.New("unknown action")
	}
	if len(targets) == 0 {
		return nil, errors.New("no ports chosen")
	}
	if len(targets) > 64 {
		return nil, errors.New("too many ports at once")
	}
	seen := map[pdu.Target]bool{}
	var unique []pdu.Target
	for _, t := range targets {
		if seen[t] {
			continue
		}
		seen[t] = true
		p, err := a.Store.PDU(ctx, t.PDUID)
		if err != nil {
			return nil, fmt.Errorf("no PDU %d", t.PDUID)
		}
		if t.Outlet < 1 || t.Outlet > 96 {
			return nil, fmt.Errorf("%s has no outlet %d", p.Name, t.Outlet)
		}
		unique = append(unique, t)
	}
	targets = unique
	settings, _ := a.Store.Settings(ctx)

	labels := make([]string, len(targets))
	for i, t := range targets {
		labels[i] = a.describe(ctx, t)
	}
	op := &Operation{ID: newID(), User: user, Kind: kind, Running: true, Started: a.Now(), Targets: targets, Detail: "Starting…",
		Title: fmt.Sprintf("%s: %s", map[string]string{"on": "Switch on", "off": "Switch off", "restart": "Restart"}[kind], strings.Join(labels, ", "))}

	a.mu.Lock()
	for _, t := range targets {
		if holder, ok := a.busy[t]; ok {
			a.mu.Unlock()
			return nil, fmt.Errorf("%w (operation %s)", ErrBusy, holder)
		}
	}
	for _, t := range targets {
		a.busy[t] = op.ID
	}
	a.ops[op.ID] = op
	started := *op // the copy for the caller is made before the operation can change
	a.opOrder = append(a.opOrder, op.ID)
	for len(a.opOrder) > 50 { // only the recent ones are kept
		delete(a.ops, a.opOrder[0])
		a.opOrder = a.opOrder[1:]
	}
	a.mu.Unlock()

	_ = a.Store.Audit(ctx, user, "power."+kind, strings.Join(labels, "; "), true)
	seq := &pdu.Sequencer{Controller: controller{a}, Label: func(t pdu.Target) string { return a.describe(context.Background(), t) }, Sleep: a.Sleep}
	go func() {
		runCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		err := seq.Run(runCtx, pdu.Operation{Kind: kind, Delay: time.Duration(settings.RestartDelaySecond) * time.Second}, targets, func(p pdu.Progress) {
			a.mu.Lock()
			op.Detail = progressText(p)
			a.mu.Unlock()
		})
		now := a.Now()
		a.mu.Lock()
		op.Running = false
		op.Finished = &now
		if err != nil {
			op.Error = err.Error()
			op.Detail = "Failed"
		} else {
			op.Detail = "Done"
		}
		for _, t := range targets {
			delete(a.busy, t)
		}
		a.mu.Unlock()
		result := "power." + kind + ".done"
		detail := strings.Join(labels, "; ")
		if err != nil {
			result = "power." + kind + ".failed"
			detail += " — " + err.Error()
		}
		_ = a.Store.Audit(context.Background(), user, result, detail, err == nil)
		ids := map[int64]bool{}
		var only []int64
		for _, t := range targets {
			if !ids[t.PDUID] {
				ids[t.PDUID] = true
				only = append(only, t.PDUID)
			}
		}
		a.PollPDUs(context.Background(), only...)
	}()
	return &started, nil
}

func progressText(p pdu.Progress) string {
	switch p.Step {
	case "switching-off":
		return "Switching off…"
	case "confirmed-off":
		return "Off confirmed by the PDU"
	case "waiting":
		return fmt.Sprintf("Waiting… %d s", p.SecondsLeft)
	case "switching-on":
		return "Switching on…"
	case "confirmed-on":
		return "On confirmed by the PDU"
	case "rolled-back":
		return "Switching back on…"
	}
	return p.Step
}

// Operation returns a copy of an operation, or false.
func (a *App) Operation(id string) (Operation, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if op, ok := a.ops[id]; ok {
		return *op, true
	}
	return Operation{}, false
}

// RecentOperations are the newest first.
func (a *App) RecentOperations() []Operation {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]Operation, 0, len(a.opOrder))
	for i := len(a.opOrder) - 1; i >= 0; i-- {
		out = append(out, *a.ops[a.opOrder[i]])
	}
	return out
}

// TestPDU tries a connection with values that are typed in the editor (nothing is saved).
func (a *App) TestPDU(ctx context.Context, p store.PDU, c store.Communities) (*pdu.Snapshot, error) {
	if c.Read == "" {
		return nil, errors.New("enter the read community")
	}
	drv, err := a.Factory(p, c)
	if err != nil {
		return nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	return drv.Poll(cctx)
}
