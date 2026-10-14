package demo

import (
	"context"
	"hash/fnv"
	"math"
	"time"

	"github.com/kblaskoff/pdu-monitor/server/internal/store"
)

// SeedIdleHistory gives every host of the database a history of CPU idle (an hour at a time, `days` back), so that the bars of the host page
// have something to show in the demo. It is not part of Seed: the tests make many databases and do not need it.
func SeedIdleHistory(ctx context.Context, st *store.Store, days int, now time.Time) error {
	hosts, err := st.Hosts(ctx)
	if err != nil {
		return err
	}
	var rows []store.IdleDelta
	for _, h := range hosts {
		f := fnv.New32a()
		_, _ = f.Write([]byte(h.Name))
		seed := float64(f.Sum32()%1000) / 1000
		base := 55 + 30*seed // how idle this host is on a normal day
		start := now.Add(-time.Duration(days) * 24 * time.Hour).Truncate(time.Hour)
		for t := start; t.Before(now); t = t.Add(time.Hour) {
			day := float64(t.Unix()) / 86400
			weekday := t.Weekday()
			idle := base + 8*math.Sin(2*math.Pi*(float64(t.Hour())-14)/24) // busier in the afternoon
			if weekday == time.Saturday || weekday == time.Sunday {
				idle += 6
			}
			idle -= 14 * math.Max(0, math.Sin(day/9+seed*6)) // slow swings over weeks
			if h.Name == "a50sf" || h.Name == "a53sf" {
				idle -= 28 // the busy hosts of the demo
			}
			if idle < 3 {
				idle = 3
			}
			if idle > 99 {
				idle = 99
			}
			low := idle - 6
			if low < 1 {
				low = 1
			}
			rows = append(rows, store.IdleDelta{HostID: h.ID, Hour: t.Unix() / 3600, Sum: idle * 60, N: 60, Min: low})
		}
	}
	for len(rows) > 0 { // in pieces: one transaction of a few hundred thousand rows is no use
		n := min(len(rows), 20000)
		if err := st.AddIdle(ctx, rows[:n]); err != nil {
			return err
		}
		rows = rows[n:]
	}
	return nil
}
