package app

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/kblaskoff/pdu-monitor/server/internal/pdu"
	"github.com/kblaskoff/pdu-monitor/server/internal/servman"
)

// ErrNoServman: servman is not configured (no servman section in the configuration file).
var ErrNoServman = errors.New("servman is not configured on this server")

// ErrNamesBusy: names are being written right now.
var ErrNamesBusy = errors.New("the names are being written right now: wait until it is finished")

const (
	maxManualNames = 500 // most outlets one press of the button writes
	maxAutoNames   = 100 // most the automatic sync writes in one round: a big difference is written over several rounds
	namePause      = 50 * time.Millisecond
	defaultSweep   = 30 * time.Second
)

// NameSelection chooses what is written: everything in the plan, one PDU, or one outlet.
type NameSelection struct {
	All    bool
	PDU    string
	Outlet int
}

// NameFailure is an outlet that did not get its name.
type NameFailure struct {
	PDU     string `json:"pdu"`
	Outlet  int    `json:"outlet"`
	Desired string `json:"desired"`
	Error   string `json:"error"`
}

// NameResult is what one writing did.
type NameResult struct {
	Written  int           `json:"written"`  // the PDU accepted the name
	Verified int           `json:"verified"` // ... and a reading afterwards shows it
	Skipped  int           `json:"skipped"`  // in the plan but blocked (no write community, no support) or left for the next round
	Failed   []NameFailure `json:"failed"`
}

// ServmanStatus is what the screen shows about the synchronisation.
type ServmanStatus struct {
	Configured bool          `json:"configured"`
	PlanAt     *time.Time    `json:"planAt,omitempty"`
	PlanError  string        `json:"planError,omitempty"`
	SyncAt     *time.Time    `json:"syncAt,omitempty"`
	SyncResult string        `json:"syncResult,omitempty"`
	Writing    bool          `json:"writing"`
	Result     *NameResult   `json:"result,omitempty"`      // the last writing: what failed
	ResultErr  string        `json:"resultError,omitempty"` // ... or why it could not start
	Plan       *servman.Plan `json:"plan,omitempty"`
}

func (a *App) pause(ctx context.Context) {
	if a.Sleep != nil {
		_ = a.Sleep(ctx, namePause)
		return
	}
	select {
	case <-ctx.Done():
	case <-time.After(namePause):
	}
}

// pduInfos describes the PDUs as the plan needs them.
func (a *App) pduInfos(ctx context.Context) ([]servman.PDUInfo, error) {
	states, configs, err := a.States(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]servman.PDUInfo, 0, len(states))
	for _, s := range states {
		info := servman.PDUInfo{ID: s.ID, Name: s.Name, Outlets: map[int]string{}, HasWrite: configs[s.ID].HasWrite}
		if s.Snapshot != nil && s.Error == "" {
			info.Read = true
			for _, o := range s.Snapshot.Outlet {
				info.Outlets[o.Number] = o.Name
			}
		}
		if drv, err := a.driver(ctx, configs[s.ID]); err == nil {
			_, info.Namer = drv.(pdu.Namer)
		}
		out = append(out, info)
	}
	return out, nil
}

// ServmanPlan asks servman and compares it with the PDUs as they were last read. Nothing is written.
func (a *App) ServmanPlan(ctx context.Context) (*servman.Plan, error) {
	if a.Servman == nil {
		return nil, ErrNoServman
	}
	plan, err := a.makePlan(ctx)
	a.snMu.Lock()
	defer a.snMu.Unlock()
	now := a.Now()
	a.planAt = &now
	if err != nil {
		a.planErr = err.Error()
		return nil, err
	}
	a.planErr = ""
	a.lastPlan = plan
	return plan, nil
}

func (a *App) makePlan(ctx context.Context) (*servman.Plan, error) {
	cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	devices, err := a.Servman.Devices(cctx)
	if err != nil {
		return nil, fmt.Errorf("servman: %w", err)
	}
	infos, err := a.pduInfos(ctx)
	if err != nil {
		return nil, err
	}
	settings, _ := a.Store.Settings(ctx)
	plan := servman.Build(devices, infos, settings.ServmanNameFormat, a.Now())
	return &plan, nil
}

// ServmanStatus is the last plan and the last writing.
func (a *App) ServmanStatus() ServmanStatus {
	a.snMu.Lock()
	defer a.snMu.Unlock()
	st := ServmanStatus{Configured: a.Servman != nil, PlanAt: a.planAt, PlanError: a.planErr, SyncAt: a.syncAt, SyncResult: a.syncRes, Writing: a.writing, Plan: a.lastPlan, Result: a.lastApply, ResultErr: a.lastApplyErr}
	return st
}

func (a *App) beginWriting() error {
	a.snMu.Lock()
	defer a.snMu.Unlock()
	if a.writing {
		return ErrNamesBusy
	}
	a.writing = true
	a.lastApply, a.lastApplyErr = nil, ""
	return nil
}

func (a *App) endWriting() {
	a.snMu.Lock()
	a.writing = false
	a.snMu.Unlock()
}

func checkSelection(sel NameSelection) error {
	if !sel.All && sel.PDU == "" {
		return errors.New("choose what to write")
	}
	return nil
}

// ApplyNames writes the names of the plan to the PDUs and returns when it is done. It always plans afresh first, so that it never
// writes an old plan. After the writing every PDU is read again and the names are compared with what was meant: a PDU that answers "ok"
// and still shows the old name is reported. Every PDU that was written to gets a line in the log.
func (a *App) ApplyNames(ctx context.Context, user string, sel NameSelection, auto bool) (*NameResult, error) {
	if a.Servman == nil {
		return nil, ErrNoServman
	}
	if err := checkSelection(sel); err != nil {
		return nil, err
	}
	if err := a.beginWriting(); err != nil {
		return nil, err
	}
	defer a.endWriting()
	return a.doApply(ctx, user, sel, auto)
}

// StartApplyNames is ApplyNames for a web request: the busy check is made at once and the writing goes on in the background (it can take
// minutes); the screen follows it with ServmanStatus.
func (a *App) StartApplyNames(user string, sel NameSelection) error {
	if a.Servman == nil {
		return ErrNoServman
	}
	if err := checkSelection(sel); err != nil {
		return err
	}
	if err := a.beginWriting(); err != nil {
		return err
	}
	go func() {
		defer a.endWriting()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		if _, err := a.doApply(ctx, user, sel, false); err != nil {
			a.logf("servman apply: %v", err)
		}
	}()
	return nil
}

func (a *App) doApply(ctx context.Context, user string, sel NameSelection, auto bool) (*NameResult, error) {
	plan, err := a.ServmanPlan(ctx)
	if err != nil {
		a.snMu.Lock()
		a.lastApplyErr = err.Error()
		a.snMu.Unlock()
		return nil, err
	}
	limit := maxManualNames
	if auto {
		limit = maxAutoNames
	}
	res := &NameResult{Failed: []NameFailure{}}
	byPDU := map[string][]servman.Change{}
	var order []string
	for _, c := range plan.Changes {
		if !sel.All && (!strings.EqualFold(c.PDU, sel.PDU) || (sel.Outlet != 0 && c.Outlet != sel.Outlet)) {
			continue
		}
		if c.Blocked != "" || limit <= 0 {
			res.Skipped++
			continue
		}
		limit--
		if _, ok := byPDU[c.PDU]; !ok {
			order = append(order, c.PDU)
		}
		byPDU[c.PDU] = append(byPDU[c.PDU], c)
	}
	sort.Strings(order)
	for _, name := range order {
		a.writePDU(ctx, user, byPDU[name], res)
	}
	summary := fmt.Sprintf("%d written, %d verified, %d failed, %d left", res.Written, res.Verified, len(res.Failed), res.Skipped)
	a.snMu.Lock()
	now := a.Now()
	a.syncAt, a.syncRes, a.lastApply = &now, summary+" (by "+user+")", res
	a.snMu.Unlock()
	if len(order) == 0 && !auto {
		_ = a.Store.Audit(ctx, user, "outlet.names", "nothing to write: "+summary, true)
	}
	_, _ = a.ServmanPlan(ctx) // planned again, so that the screen shows what is left
	return res, nil
}

func (a *App) writePDU(ctx context.Context, user string, changes []servman.Change, res *NameResult) {
	first := changes[0]
	before := len(res.Failed)
	fail := func(c servman.Change, msg string) {
		res.Failed = append(res.Failed, NameFailure{PDU: c.PDU, Outlet: c.Outlet, Desired: c.Desired, Error: msg})
	}
	p, err := a.Store.PDU(ctx, first.PDUID)
	if err != nil {
		for _, c := range changes {
			fail(c, "no such PDU")
		}
		return
	}
	drv, err := a.driver(ctx, p)
	namer, ok := drv.(pdu.Namer)
	if err != nil || !ok {
		msg := "the driver of this PDU cannot rename outlets"
		if err != nil {
			msg = err.Error()
		}
		for _, c := range changes {
			fail(c, msg)
		}
		return
	}
	var done []servman.Change
	var lines []string
	failures := 0
	for i, c := range changes {
		if failures >= 3 { // a PDU that does not answer is not asked 20 more times
			for _, rest := range changes[i:] {
				fail(rest, "skipped after repeated errors on this PDU")
			}
			break
		}
		cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		err := namer.SetOutletName(cctx, c.Outlet, c.Desired)
		cancel()
		if err != nil {
			failures++
			fail(c, err.Error())
			continue
		}
		failures = 0
		res.Written++
		done = append(done, c)
		lines = append(lines, fmt.Sprintf("#%d %q→%q", c.Outlet, c.Current, c.Desired))
		a.pause(ctx)
	}
	if len(done) > 0 {
		a.PollPDUs(ctx, p.ID)
		a.mu.Lock()
		snap := a.readings[p.ID].Snapshot
		a.mu.Unlock()
		now := map[int]string{}
		if snap != nil {
			for _, o := range snap.Outlet {
				now[o.Number] = o.Name
			}
		}
		for _, c := range done {
			if strings.TrimSpace(now[c.Outlet]) == c.Desired {
				res.Verified++
			} else {
				fail(c, fmt.Sprintf("the PDU accepted the name but shows %q", now[c.Outlet]))
			}
		}
	}
	detail := first.PDU + ": " + strings.Join(lines, ", ")
	if len(detail) > 600 {
		detail = detail[:600] + "…"
	}
	if len(res.Failed) > 0 {
		detail += fmt.Sprintf(" — %d failed", len(res.Failed))
	}
	_ = a.Store.Audit(ctx, user, "outlet.names", detail, len(res.Failed) == before)
}

// servmanLoop fetches and writes by the intervals of the settings (read again every round, so that a change takes effect at once).
func (a *App) servmanLoop(ctx context.Context) {
	var lastPlan, lastSync time.Time
	sweep := a.ServmanSweep
	if sweep <= 0 {
		sweep = defaultSweep
	}
	t := time.NewTicker(sweep)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		st, err := a.Store.Settings(ctx)
		if err != nil {
			continue
		}
		now := a.Now()
		if st.ServmanSyncMinutes > 0 && now.Sub(lastSync) >= time.Duration(st.ServmanSyncMinutes)*time.Minute {
			lastSync, lastPlan = now, now
			if res, err := a.ApplyNames(ctx, "servman-sync", NameSelection{All: true}, true); err != nil && !errors.Is(err, ErrNamesBusy) {
				a.logf("servman sync: %v", err)
			} else if res != nil && (res.Written > 0 || len(res.Failed) > 0) {
				a.logf("servman sync: %d written, %d verified, %d failed", res.Written, res.Verified, len(res.Failed))
			}
			continue
		}
		if st.ServmanPollMinutes > 0 && now.Sub(lastPlan) >= time.Duration(st.ServmanPollMinutes)*time.Minute {
			lastPlan = now
			if _, err := a.ServmanPlan(ctx); err != nil {
				a.logf("servman: %v", err)
			}
		}
	}
}
