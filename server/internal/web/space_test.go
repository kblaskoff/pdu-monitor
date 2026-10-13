package web

import (
	"testing"

	"github.com/kblaskoff/pdu-monitor/server/internal/hostinfo"
)

func f64(v float64) *float64 { return &v }
func i64(v int64) *int64     { return &v }

// The same rule for ZFS and LVM: yellow under 20% free, red under 10%; a thin pool is yellow from 80% full and red from 90%.
func TestSpaceLevel(t *testing.T) {
	const tb = int64(1_000_000_000_000)
	pool := func(name string, avail, total int64, health string) hostinfo.Pool {
		return hostinfo.Pool{Name: name, Size: total * 2, Free: total * 2, Health: health, UsableAvail: i64(avail), UsableTotal: i64(total)}
	}
	vg := func(name string, free, size int64, fill ...hostinfo.FillVolume) hostinfo.VG {
		return hostinfo.VG{Name: name, Free: free, Size: size, Fill: fill}
	}
	cases := []struct {
		name  string
		brief hostBrief
		want  string
	}{
		{"no storage reported", hostBrief{}, ""},
		{"zfs plenty", hostBrief{Pools: []hostinfo.Pool{pool("pool", 50*tb, 100*tb, "ONLINE")}}, "ok"},
		{"zfs exactly 20% is still fine", hostBrief{Pools: []hostinfo.Pool{pool("pool", 20*tb, 100*tb, "ONLINE")}}, "ok"},
		{"zfs just under 20%", hostBrief{Pools: []hostinfo.Pool{pool("pool", 19*tb, 100*tb, "ONLINE")}}, "warning"},
		{"zfs exactly 10% is yellow", hostBrief{Pools: []hostinfo.Pool{pool("pool", 10*tb, 100*tb, "ONLINE")}}, "warning"},
		{"zfs under 10%", hostBrief{Pools: []hostinfo.Pool{pool("pool", 9*tb, 100*tb, "ONLINE")}}, "over"},
		{"zfs degraded with room", hostBrief{Pools: []hostinfo.Pool{pool("pool", 80*tb, 100*tb, "DEGRADED")}}, "over"},
		{"zfs without usable numbers uses the raw ones", hostBrief{Pools: []hostinfo.Pool{{Name: "p", Size: 100, Free: 5, Health: "ONLINE"}}}, "over"},
		{"lvm plenty", hostBrief{LVM: []hostinfo.VG{vg("am", 8*tb, 20*tb)}}, "ok"},
		{"lvm 15% free", hostBrief{LVM: []hostinfo.VG{vg("am", 3*tb, 20*tb)}}, "warning"},
		{"lvm 5% free", hostBrief{LVM: []hostinfo.VG{vg("am", 1*tb, 20*tb)}}, "over"},
		{"lvm with room but a thin pool 85% full", hostBrief{LVM: []hostinfo.VG{vg("am", 10*tb, 20*tb, hostinfo.FillVolume{Name: "t", Kind: "thin-pool", DataPct: f64(85)})}}, "warning"},
		{"lvm with room but thin metadata 95% full", hostBrief{LVM: []hostinfo.VG{vg("am", 10*tb, 20*tb, hostinfo.FillVolume{Name: "t", Kind: "thin-pool", DataPct: f64(10), MetaPct: f64(95)})}}, "over"},
		{"a thin pool whose percentages are unknown does not count", hostBrief{LVM: []hostinfo.VG{vg("am", 10*tb, 20*tb, hostinfo.FillVolume{Name: "t", Kind: "thin-pool"})}}, "ok"},
		{"the worst group decides, not the sum", hostBrief{LVM: []hostinfo.VG{vg("a", 90*tb, 100*tb), vg("b", 1*tb, 100*tb)}}, "over"},
		{"zfs fine, lvm short", hostBrief{Pools: []hostinfo.Pool{pool("pool", 60*tb, 100*tb, "ONLINE")}, LVM: []hostinfo.VG{vg("am", 3*tb, 20*tb)}}, "warning"},
	}
	for _, c := range cases {
		b := c.brief
		b.setSpace()
		if b.Space != c.want {
			t.Errorf("%s: %q, want %q (%s)", c.name, b.Space, c.want, b.SpaceWhy)
		}
		if c.want == "warning" || c.want == "over" {
			if b.SpaceWhy == "" {
				t.Errorf("%s: no reason given", c.name)
			}
		}
		if c.want != "" && b.SpaceFree == nil && c.name != "a thin pool whose percentages are unknown does not count" {
			t.Errorf("%s: no free share", c.name)
		}
	}
	// the reason names the culprit
	b := hostBrief{LVM: []hostinfo.VG{vg("a", 90*tb, 100*tb), vg("bad", 1*tb, 100*tb)}}
	b.setSpace()
	if b.SpaceWhy != "bad: 1% free" || b.SpaceFree == nil || *b.SpaceFree != 0.01 {
		t.Errorf("%q %v", b.SpaceWhy, b.SpaceFree)
	}
}
