package hostinfo

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/kblaskoff/pdu-monitor/server/internal/store"
)

// State is the last answer of one host.
type State struct {
	Host    store.Host
	Status  *Status
	Error   string
	Fetched time.Time
}

// HostSource lists the hosts to poll (the store).
type HostSource interface {
	Hosts(ctx context.Context) ([]store.Host, error)
}

type topKey struct {
	host   int64
	window string
}

type topEntry struct {
	top     *Top
	fetched time.Time
}

// Poller asks all the hosts for their status from time to time and keeps the last answers in memory.
type Poller struct {
	Source   HostSource
	Runner   Runner
	Parallel int
	TopTTL   time.Duration
	Now      func() time.Time
	Log      func(string)

	mu     sync.Mutex
	states map[int64]*State
	tops   map[topKey]topEntry
}

func NewPoller(src HostSource, r Runner) *Poller {
	return &Poller{Source: src, Runner: r, Parallel: 8, TopTTL: 30 * time.Second, Now: time.Now,
		states: map[int64]*State{}, tops: map[topKey]topEntry{}}
}

func (p *Poller) logf(msg string) {
	if p.Log != nil {
		p.Log(msg)
	}
}

// PollAll reads every enabled host (a few at the same time).
func (p *Poller) PollAll(ctx context.Context) {
	hosts, err := p.Source.Hosts(ctx)
	if err != nil {
		p.logf("hosts: " + err.Error())
		return
	}
	current := map[int64]bool{}
	sem := make(chan struct{}, max(p.Parallel, 1))
	var wg sync.WaitGroup
	for _, h := range hosts {
		current[h.ID] = true
		if !h.Enabled {
			p.set(h, nil, "disabled")
			continue
		}
		if h.Collector != "" { // a collector reads this one and pushes the readings (Ingest)
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(h store.Host) {
			defer wg.Done()
			defer func() { <-sem }()
			p.PollOne(ctx, h)
		}(h)
	}
	wg.Wait()
	p.mu.Lock()
	for id := range p.states { // hosts that were deleted
		if !current[id] {
			delete(p.states, id)
		}
	}
	p.mu.Unlock()
}

// PollOne reads one host now.
func (p *Poller) PollOne(ctx context.Context, h store.Host) {
	cctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	out, err := p.Runner.Run(cctx, h, "status")
	if err != nil {
		p.logf("host " + h.Name + ": " + err.Error())
		p.set(h, nil, err.Error())
		return
	}
	status, err := ParseStatus(out)
	if err != nil {
		p.logf("host " + h.Name + ": " + err.Error())
		p.set(h, nil, err.Error())
		return
	}
	p.set(h, status, "")
}

func (p *Poller) set(h store.Host, s *Status, errText string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	old := p.states[h.ID]
	st := &State{Host: h, Status: s, Error: errText, Fetched: p.Now()}
	if s == nil && old != nil && old.Status != nil && errText != "disabled" {
		st.Status = old.Status // the last good reading stays visible (the screen shows its age and the error)
		st.Fetched = old.Fetched
	}
	p.states[h.ID] = st
}

// Ingest stores a reading that a collector pushed. A nil status with an error keeps the last good reading visible, like a failed poll.
func (p *Poller) Ingest(h store.Host, s *Status, errText string) { p.set(h, s, errText) }

// IngestTop stores the busiest VMs of a window that a collector pushed.
func (p *Poller) IngestTop(h store.Host, window string, t *Top) {
	p.mu.Lock()
	p.tops[topKey{h.ID, window}] = topEntry{t, p.Now()}
	p.mu.Unlock()
}

// CachedTop is the last busiest-VMs answer for a host that is read by a collector, and when it arrived.
func (p *Poller) CachedTop(h store.Host, window string) (*Top, time.Time, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.tops[topKey{h.ID, window}]
	return e.top, e.fetched, ok
}

// States are the last answers, by host name.
func (p *Poller) States() []State {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]State, 0, len(p.states))
	for _, s := range p.states {
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Host.Name < out[j].Host.Name })
	return out
}

func (p *Poller) State(id int64) (State, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if s, ok := p.states[id]; ok {
		return *s, true
	}
	return State{}, false
}

// Top asks a host for its busiest VMs in a window. Answers are kept for a short time, so that many people looking at the same page
// do not make the host work.
func (p *Poller) Top(ctx context.Context, h store.Host, window string) (*Top, error) {
	if !ValidWindow(window) {
		return nil, errors.New("unknown period")
	}
	key := topKey{h.ID, window}
	p.mu.Lock()
	if e, ok := p.tops[key]; ok && p.Now().Sub(e.fetched) < p.TopTTL {
		p.mu.Unlock()
		return e.top, nil
	}
	p.mu.Unlock()
	cctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	out, err := p.Runner.Run(cctx, h, "top "+window)
	if err != nil {
		return nil, err
	}
	top, err := ParseTop(out, window)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	p.tops[key] = topEntry{top, p.Now()}
	p.mu.Unlock()
	return top, nil
}

// Run polls forever until the context ends. interval is read each time so that a change of the settings takes effect.
func (p *Poller) Run(ctx context.Context, interval func() time.Duration) {
	for {
		p.PollAll(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval()):
		}
	}
}
