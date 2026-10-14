package app

import (
	"context"
	"sync"
	"time"

	"github.com/kblaskoff/pdu-monitor/server/internal/hostinfo"
	"github.com/kblaskoff/pdu-monitor/server/internal/store"
)

const (
	idleFlushEvery = 5 * time.Minute
	idleKeepDays   = 400 // a year and a bit: the weekly bars of a year are complete
)

// idleKeeper collects the idle readings of the hosts into hours. Nothing is asked of the hosts for it: it uses the readings that are taken
// anyway (one a minute). Per host there is one number per minute in memory and one row per hour in the database.
type idleKeeper struct {
	mu   sync.Mutex
	acc  map[int64]*idleAcc
	last map[int64]int64 // the time of the last reading that was counted, so that the same reading is not counted twice
	done []store.IdleDelta
}

type idleAcc struct {
	hour int64
	sum  float64
	n    int
	min  float64
}

func newIdleKeeper() *idleKeeper {
	return &idleKeeper{acc: map[int64]*idleAcc{}, last: map[int64]int64{}}
}

// record counts a reading of a host (the idle percent at the time the agent took the sample).
func (k *idleKeeper) record(hostID int64, t int64, idle float64) {
	if idle < 0 || idle > 100 || t <= 0 {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if t <= k.last[hostID] { // the same reading again, or an older one
		return
	}
	k.last[hostID] = t
	hour := t / 3600
	a := k.acc[hostID]
	if a != nil && a.hour != hour { // a new hour: the old one is done
		if a.n > 0 {
			k.done = append(k.done, store.IdleDelta{HostID: hostID, Hour: a.hour, Sum: a.sum, N: a.n, Min: a.min})
		}
		a = nil
	}
	if a == nil {
		a = &idleAcc{hour: hour, min: idle}
		k.acc[hostID] = a
	}
	a.sum += idle
	a.n++
	if idle < a.min {
		a.min = idle
	}
}

// take returns what is to be stored and starts counting again (the part that was taken is not counted twice: it is added in the database).
func (k *idleKeeper) take() []store.IdleDelta {
	k.mu.Lock()
	defer k.mu.Unlock()
	out := k.done
	k.done = nil
	for id, a := range k.acc {
		if a.n > 0 {
			out = append(out, store.IdleDelta{HostID: id, Hour: a.hour, Sum: a.sum, N: a.n, Min: a.min})
			a.sum, a.n, a.min = 0, 0, 100
		}
	}
	return out
}

// recordIdle is called with every new reading of a host.
func (a *App) recordIdle(h store.Host, s *hostinfo.Status) {
	if s == nil || s.CPU.IdlePct == nil {
		return
	}
	a.idle.record(h.ID, s.T, *s.CPU.IdlePct)
}

// FlushIdle stores the idle readings that are waiting.
func (a *App) FlushIdle(ctx context.Context) {
	rows := a.idle.take()
	if len(rows) == 0 {
		return
	}
	if err := a.Store.AddIdle(ctx, rows); err != nil {
		a.logf("idle history: %v", err)
	}
}

func (a *App) idleLoop(ctx context.Context) {
	t := time.NewTicker(idleFlushEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			fctx, cancel := context.WithTimeout(context.Background(), 10*time.Second) // what is waiting is stored before the program ends
			a.FlushIdle(fctx)
			cancel()
			return
		case <-t.C:
			a.FlushIdle(ctx)
		}
	}
}
