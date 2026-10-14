package app_test

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/kblaskoff/pdu-monitor/server/internal/app"
	"github.com/kblaskoff/pdu-monitor/server/internal/hostinfo"
	"github.com/kblaskoff/pdu-monitor/server/internal/store"
)

func status(t int64, idle *float64) *hostinfo.Status {
	s := &hostinfo.Status{V: 1, Host: "h", T: t}
	s.CPU.IdlePct = idle
	return s
}

func f(v float64) *float64 { return &v }

func TestIdleReadingsBecomeHours(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	poller := hostinfo.NewPoller(e.st, nil)
	a := app.New(e.st, poller, nil)
	host, err := e.st.SaveHost(ctx, store.Host{Name: "a1", Address: "10.0.0.1", Port: 22, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC).Unix()
	poller.Ingest(host, status(t0+10, f(80)), "")
	poller.Ingest(host, status(t0+10, f(80)), "") // the same reading polled again is one reading
	poller.Ingest(host, status(t0+70, f(60)), "")
	poller.Ingest(host, status(t0+130, nil), "")    // a first sample has no idle yet
	poller.Ingest(host, status(t0+190, f(150)), "") // nonsense is not counted
	poller.Ingest(host, status(t0+250, f(-3)), "")
	poller.Ingest(host, nil, "the host does not answer") // an error is not a reading
	poller.Ingest(host, status(t0+3700, f(40)), "")      // the next hour
	a.FlushIdle(ctx)
	hours, _ := e.st.IdleHours(ctx, host.ID, time.Unix(t0, 0).Add(-time.Hour), time.Unix(t0, 0).Add(3*time.Hour))
	if len(hours) != 2 || hours[0].N != 2 || math.Abs(hours[0].Avg-70) > 1e-9 || hours[0].Min != 60 || hours[1].N != 1 || hours[1].Avg != 40 {
		t.Fatalf("%+v", hours)
	}
	// flushed again with nothing new: nothing is added twice
	a.FlushIdle(ctx)
	poller.Ingest(host, status(t0+3760, f(20)), "") // the same hour goes on after a flush: it is added to the hour
	a.FlushIdle(ctx)
	hours, _ = e.st.IdleHours(ctx, host.ID, time.Unix(t0, 0).Add(-time.Hour), time.Unix(t0, 0).Add(3*time.Hour))
	if len(hours) != 2 || hours[0].N != 2 || hours[1].N != 2 || math.Abs(hours[1].Avg-30) > 1e-9 || hours[1].Min != 20 {
		t.Fatalf("%+v", hours)
	}
	// an older reading than the last one is ignored (a collector that sends late)
	poller.Ingest(host, status(t0+100, f(1)), "")
	a.FlushIdle(ctx)
	if h2, _ := e.st.IdleHours(ctx, host.ID, time.Unix(t0, 0), time.Unix(t0, 0)); h2[0].N != 2 {
		t.Errorf("%+v", h2)
	}
	// a host that was deleted while readings were waiting does not make a flush fail
	poller.Ingest(host, status(t0+3820, f(10)), "")
	if err := e.st.DeleteHost(ctx, host.ID); err != nil {
		t.Fatal(err)
	}
	a.FlushIdle(ctx)
}
