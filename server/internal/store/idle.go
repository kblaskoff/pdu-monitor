package store

import (
	"context"
	"sort"
	"strings"
	"time"
)

// IdleDelta is what was read for a host in one hour since the last time it was stored: the sum and the number of the idle readings (percent)
// and the lowest one. It is added to what is there.
type IdleDelta struct {
	HostID int64
	Hour   int64 // unix time / 3600
	Sum    float64
	N      int
	Min    float64
}

// IdleHour is one stored hour of a host.
type IdleHour struct {
	Hour int64   `json:"hour"`
	Avg  float64 `json:"avg"`
	Min  float64 `json:"min"` // the lowest single reading of the hour
	N    int     `json:"n"`
}

// AddIdle adds readings to the hours. A host that was deleted meanwhile is skipped.
func (s *Store) AddIdle(ctx context.Context, rows []IdleDelta) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO host_idle (host_id, hour, sum, n, min) VALUES (?,?,?,?,?)
		ON CONFLICT(host_id, hour) DO UPDATE SET sum = sum + excluded.sum, n = n + excluded.n, min = MIN(min, excluded.min)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, r := range rows {
		if r.N <= 0 {
			continue
		}
		_, err := stmt.ExecContext(ctx, r.HostID, r.Hour, r.Sum, r.N, r.Min)
		if err != nil && !strings.Contains(err.Error(), "FOREIGN KEY") {
			return err
		}
	}
	return tx.Commit()
}

// IdleHours are the stored hours of a host in [from, to], oldest first.
func (s *Store) IdleHours(ctx context.Context, hostID int64, from, to time.Time) ([]IdleHour, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT hour, sum, n, min FROM host_idle WHERE host_id = ? AND hour >= ? AND hour <= ? ORDER BY hour`,
		hostID, from.Unix()/3600, to.Unix()/3600)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []IdleHour
	for rows.Next() {
		var h IdleHour
		var sum float64
		if err := rows.Scan(&h.Hour, &sum, &h.N, &h.Min); err != nil {
			return nil, err
		}
		if h.N > 0 {
			h.Avg = sum / float64(h.N)
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// PruneIdle removes the hours before a time.
func (s *Store) PruneIdle(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM host_idle WHERE hour < ?`, before.Unix()/3600)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// IdleBucket is a day or a week of idle: the average of all readings in it (weighted by their number), the lowest hourly average and how
// many readings it has. N is 0 when there is nothing for it (the host was not read then).
type IdleBucket struct {
	Start int64    `json:"t"`   // unix time of the beginning (midnight of the day, or of the Monday), in the time zone of the viewer
	Avg   *float64 `json:"avg"` // nil without readings
	Min   *float64 `json:"min"`
	N     int      `json:"n"`
}

// BucketIdle puts hours into the last `count` days or weeks (unit "day" or "week"; a week begins on Monday) up to and including the one
// that contains now. offsetMinutes is the one of the viewer as JavaScript gives it (UTC minus local, so UTC+2 is -120), so that a day is a
// day for the person who looks at it. The first bucket is the oldest.
func BucketIdle(hours []IdleHour, unit string, offsetMinutes, count int, now time.Time) []IdleBucket {
	step := int64(86400)
	if unit == "week" {
		step = 7 * 86400
	}
	// day 0 of the Unix epoch is a Thursday: a week that begins on Monday begins 3 days before a multiple of 7
	origin := int64(0)
	if unit == "week" {
		origin = -3 * 86400
	}
	shift := -int64(offsetMinutes) * 60 // local = UTC + shift
	index := func(utc int64) int64 {
		local := utc + shift - origin
		i := local / step
		if local < 0 && local%step != 0 {
			i--
		}
		return i
	}
	last := index(now.Unix())
	type acc struct {
		sum  float64
		n    int
		min  float64
		seen bool
	}
	accs := map[int64]*acc{}
	for _, h := range hours {
		i := index(h.Hour * 3600)
		if i > last || i <= last-int64(count) {
			continue
		}
		a := accs[i]
		if a == nil {
			a = &acc{min: h.Avg}
			accs[i] = a
		}
		a.sum += h.Avg * float64(h.N)
		a.n += h.N
		if h.Avg < a.min {
			a.min = h.Avg
		}
	}
	keys := make([]int64, 0, count)
	for i := last - int64(count) + 1; i <= last; i++ {
		keys = append(keys, i)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	out := make([]IdleBucket, 0, count)
	for _, i := range keys {
		b := IdleBucket{Start: i*step + origin - shift}
		if a := accs[i]; a != nil && a.n > 0 {
			avg, min := a.sum/float64(a.n), a.min
			b.Avg, b.Min, b.N = &avg, &min, a.n
		}
		out = append(out, b)
	}
	return out
}
