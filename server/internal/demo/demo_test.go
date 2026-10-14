package demo_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/kblaskoff/pdu-monitor/server/internal/app"
	"github.com/kblaskoff/pdu-monitor/server/internal/demo"
	"github.com/kblaskoff/pdu-monitor/server/internal/hostinfo"
	"github.com/kblaskoff/pdu-monitor/server/internal/pdu"
	"github.com/kblaskoff/pdu-monitor/server/internal/store"
)

func TestTheDemoSeedsAndPollsLikeTheRealThing(t *testing.T) {
	dir := t.TempDir()
	key, _ := store.LoadOrCreateKey(filepath.Join(dir, "k"))
	sec, _ := store.NewSecrets(key)
	st, err := store.Open(filepath.Join(dir, "db"), sec)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	lab, err := demo.Seed(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	racks, _ := st.Racks(ctx)
	pdus, _ := st.PDUs(ctx)
	hosts, _ := st.Hosts(ctx)
	if len(racks) != 4 || len(pdus) != 8 || len(hosts) != 15 {
		t.Fatalf("%d racks %d pdus %d hosts", len(racks), len(pdus), len(hosts))
	}
	a := app.New(st, hostinfo.NewPoller(st, lab.Runner()), lab.Factory)
	a.PollPDUs(ctx)
	a.Hosts.PollAll(ctx)
	states, _, _ := a.States(ctx)
	byRack := map[string]float64{}
	for _, s := range states {
		if s.Snapshot == nil || s.Error != "" {
			t.Fatalf("%s: %+v", s.Name, s)
		}
		byRack[s.Name[1:len(s.Name)-1]] += *s.Amps()
	}
	if byRack["20F"] <= 24 || byRack["100"] > 19 {
		t.Fatalf("the demo shows an over-limit rack and a quiet one: %v", byRack)
	}
	for _, s := range a.Hosts.States() {
		if s.Status == nil || s.Error != "" || len(s.Status.VMs.List) == 0 || s.Status.CPU.Cores == nil {
			t.Fatalf("%s: %+v", s.Host.Name, s)
		}
	}
	top, err := a.Hosts.Top(ctx, hosts[0], "6h")
	if err != nil || len(top.VMs) == 0 || top.VMs[0].CPUS < top.VMs[len(top.VMs)-1].CPUS {
		t.Fatalf("%+v %v", top, err)
	}
	// the demo servers are the ones of the racks, and a dual-feed server really has two ports
	rackStates := []pdu.State{}
	for _, s := range states {
		if s.Name == "P200A" || s.Name == "P200B" {
			rackStates = append(rackStates, s)
		}
	}
	servers := pdu.Servers(rackStates)
	if len(servers) != 13 || len(servers[0].Ports) != 2 {
		t.Fatalf("%d servers", len(servers))
	}
	// seeding twice must not duplicate
	if _, err := demo.Seed(ctx, st); err != nil {
		t.Fatal(err)
	}
	if again, _ := st.Racks(ctx); len(again) != 4 {
		t.Fatal("seeded twice")
	}
}

func TestSeedIdleHistory(t *testing.T) {
	dir := t.TempDir()
	key, _ := store.LoadOrCreateKey(filepath.Join(dir, "k"))
	sec, _ := store.NewSecrets(key)
	st, err := store.Open(filepath.Join(dir, "db"), sec)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if _, err := demo.Seed(ctx, st); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	if err := demo.SeedIdleHistory(ctx, st, 20, now); err != nil {
		t.Fatal(err)
	}
	hosts, _ := st.Hosts(ctx)
	hours, _ := st.IdleHours(ctx, hosts[0].ID, now.Add(-30*24*time.Hour), now)
	if len(hours) < 470 || len(hours) > 481 {
		t.Fatalf("%d hours", len(hours))
	}
	for _, h := range hours {
		if h.Avg < 1 || h.Avg > 100 || h.Min > h.Avg || h.N != 60 {
			t.Fatalf("%+v", h)
		}
	}
	if err := demo.SeedIdleHistory(ctx, st, 20, now); err != nil { // twice: the hours are added, the picture does not break
		t.Fatal(err)
	}
}
