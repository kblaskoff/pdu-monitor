package store_test

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/kblaskoff/pdu-monitor/server/internal/store"
)

func hourOf(t time.Time) int64 { return t.Unix() / 3600 }

func TestIdleHoursAreAddedAndPruned(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	h, err := s.SaveHost(ctx, store.Host{Name: "a1", Address: "10.0.0.1", Port: 22, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)
	// two storings of the same hour are added (the readings of an hour reach the table in parts)
	if err := s.AddIdle(ctx, []store.IdleDelta{{HostID: h.ID, Hour: hourOf(t0), Sum: 160, N: 2, Min: 70}, {HostID: h.ID, Hour: hourOf(t0.Add(time.Hour)), Sum: 50, N: 1, Min: 50}}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddIdle(ctx, []store.IdleDelta{{HostID: h.ID, Hour: hourOf(t0), Sum: 60, N: 1, Min: 60}, {HostID: 9999, Hour: hourOf(t0), Sum: 1, N: 1, Min: 1}, {HostID: h.ID, Hour: hourOf(t0), N: 0}}); err != nil {
		t.Fatalf("a deleted host must not stop the others: %v", err)
	}
	if err := s.AddIdle(ctx, []store.IdleDelta{{HostID: h.ID, Hour: hourOf(t0), Sum: 90, N: 1, Min: 90}}); err != nil { // a later part with a higher minimum
		t.Fatal(err)
	}
	hours, err := s.IdleHours(ctx, h.ID, t0.Add(-time.Hour), t0.Add(3*time.Hour))
	if err != nil || len(hours) != 2 {
		t.Fatalf("%v %+v", err, hours)
	}
	first := hours[0]
	if first.Hour != hourOf(t0) || first.N != 4 || math.Abs(first.Avg-310.0/4) > 1e-9 || first.Min != 60 {
		t.Errorf("%+v", first)
	}
	if got, _ := s.IdleHours(ctx, h.ID, t0.Add(30*time.Minute), t0.Add(time.Hour)); len(got) != 2 { // the hour of "from" and "to" are in
		t.Errorf("the range: %+v", got)
	}
	// older hours go
	if n, err := s.PruneIdle(ctx, t0.Add(30*time.Minute)); err != nil || n != 0 {
		t.Errorf("%d %v", n, err) // the hour 10:00 begins before 10:30 and is the hour of the limit: kept
	}
	if n, _ := s.PruneIdle(ctx, t0.Add(time.Hour)); n != 1 {
		t.Errorf("pruned %d", n)
	}
	// the hours of a deleted host are deleted with it
	if err := s.DeleteHost(ctx, h.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.IdleHours(ctx, h.ID, t0, t0.Add(48*time.Hour)); len(got) != 0 {
		t.Errorf("%+v", got)
	}
}

func hours(t0 time.Time, avg float64, n int, hoursCount int) []store.IdleHour {
	var out []store.IdleHour
	for i := 0; i < hoursCount; i++ {
		out = append(out, store.IdleHour{Hour: hourOf(t0.Add(time.Duration(i) * time.Hour)), Avg: avg, Min: avg, N: n})
	}
	return out
}

func val(p *float64) float64 {
	if p == nil {
		return -1
	}
	return *p
}

func TestIdleDaysFollowTheTimeZoneOfTheViewer(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 30, 0, 0, time.UTC) // a Wednesday
	// the reading of 23:00 UTC on the 12th is the 13th at 01:00 for somebody at UTC+2 (JavaScript says -120)
	h := []store.IdleHour{{Hour: hourOf(time.Date(2026, 10, 12, 22, 0, 0, 0, time.UTC)), Avg: 10, Min: 10, N: 60}, {Hour: hourOf(time.Date(2026, 10, 12, 23, 0, 0, 0, time.UTC)), Avg: 90, Min: 90, N: 60}}
	utc := store.BucketIdle(h, "day", 0, 14, now)
	if len(utc) != 14 || utc[13].Start != time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC).Unix() || utc[12].Start != time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC).Unix() {
		t.Fatalf("%+v", utc)
	}
	if b := utc[11]; b.N != 120 || val(b.Avg) != 50 || val(b.Min) != 10 { // both hours are on the 12th in UTC
		t.Errorf("UTC: %+v", b)
	}
	plus2 := store.BucketIdle(h, "day", -120, 14, now)
	if plus2[11].N != 0 {
		t.Errorf("the 12th at UTC+2 has nothing: %+v", plus2[11])
	}
	// at UTC+2 both readings (00:00 and 01:00 on the 13th) are on the 13th
	if b := plus2[12]; b.N != 120 || val(b.Avg) != 50 {
		t.Errorf("UTC+2: %+v %+v", plus2[11], b)
	}
	if plus2[13].Start != time.Date(2026, 10, 13, 22, 0, 0, 0, time.UTC).Unix() { // midnight of the 14th at UTC+2 is 22:00 UTC on the 13th
		t.Errorf("the start of the last day: %d", plus2[13].Start)
	}
	// west of Greenwich (UTC-5 is +300)
	minus5 := store.BucketIdle(nil, "day", 300, 3, time.Date(2026, 10, 14, 3, 0, 0, 0, time.UTC)) // it is still the 13th there (22:00)
	if minus5[2].Start != time.Date(2026, 10, 13, 5, 0, 0, 0, time.UTC).Unix() {
		t.Errorf("%d", minus5[2].Start)
	}
}

func TestIdleBucketsAreFilledAndWeighted(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	var h []store.IdleHour
	h = append(h, hours(time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC), 80, 60, 12)...)                                 // today: 80 %
	h = append(h, hours(time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC), 20, 60, 24)...)                                 // four days ago
	h = append(h, store.IdleHour{Hour: hourOf(time.Date(2026, 10, 10, 5, 0, 0, 0, time.UTC)), Avg: 5, Min: 1, N: 600}) // the same day: a busy hour with many readings
	h = append(h, hours(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), 99, 60, 24)...)                                   // older than 14 days: not in
	b := store.BucketIdle(h, "day", 0, 14, now)
	if len(b) != 14 {
		t.Fatal(len(b))
	}
	if b[13].N != 720 || val(b[13].Avg) != 80 {
		t.Errorf("today: %+v", b[13])
	}
	four := b[9] // the 10th
	wantAvg := (20.0*60*24 + 5.0*600) / (60*24 + 600)
	if four.N != 60*24+600 || math.Abs(val(four.Avg)-wantAvg) > 1e-9 || val(four.Min) != 5 {
		t.Errorf("the 10th: %+v want avg %v", four, wantAvg)
	}
	empty := 0
	for _, x := range b {
		if x.N == 0 {
			empty++
			if x.Avg != nil || x.Min != nil {
				t.Errorf("an empty day has no numbers: %+v", x)
			}
		}
	}
	if empty != 12 {
		t.Errorf("%d empty days", empty)
	}
	for i := 1; i < len(b); i++ {
		if b[i].Start-b[i-1].Start != 86400 {
			t.Errorf("the days are consecutive: %d", i)
		}
	}
}

func TestIdleWeeksBeginOnMonday(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC) // Wednesday 14 October; its week began on Monday the 12th
	var h []store.IdleHour
	h = append(h, hours(time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC), 30, 60, 24)...) // Sunday the 11th: the week before
	h = append(h, hours(time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC), 70, 60, 24)...) // Monday the 12th: this week
	b := store.BucketIdle(h, "week", 0, 53, now)
	if len(b) != 53 {
		t.Fatal(len(b))
	}
	if b[52].Start != time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC).Unix() || b[51].Start != time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC).Unix() {
		t.Errorf("%d %d", b[52].Start, b[51].Start)
	}
	if val(b[52].Avg) != 70 || val(b[51].Avg) != 30 || b[52].N != 1440 {
		t.Errorf("%+v %+v", b[51], b[52])
	}
	if time.Unix(b[0].Start, 0).UTC().Weekday() != time.Monday {
		t.Errorf("the first week begins on %v", time.Unix(b[0].Start, 0).UTC().Weekday())
	}
	// for somebody at UTC+2 Sunday 22:00 UTC is already Monday
	h2 := []store.IdleHour{{Hour: hourOf(time.Date(2026, 10, 11, 22, 0, 0, 0, time.UTC)), Avg: 40, Min: 40, N: 60}}
	if b := store.BucketIdle(h2, "week", -120, 53, now); val(b[52].Avg) != 40 || b[52].Start != time.Date(2026, 10, 11, 22, 0, 0, 0, time.UTC).Unix() {
		t.Errorf("%+v", b[52])
	}
	if b := store.BucketIdle(nil, "week", 0, 0, now); len(b) != 0 {
		t.Errorf("%+v", b)
	}
}
