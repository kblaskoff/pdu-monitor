// Package demo fills an empty database with sample racks, PDUs and hypervisors and answers for them without any network: for trying the
// interface and for the screenshots. Nothing here is ever used with real devices.
package demo

import (
	"context"
	"encoding/json"
	"fmt"
	"golang.org/x/crypto/ssh"
	"hash/fnv"
	"math"
	"sync"
	"time"

	"github.com/kblaskoff/pdu-monitor/server/internal/hostinfo"
	"github.com/kblaskoff/pdu-monitor/server/internal/pdu"
	"github.com/kblaskoff/pdu-monitor/server/internal/store"
)

// Lab is what Seed returns: drivers for the sample PDUs and a runner for the sample hosts.
type Lab struct {
	mu   sync.Mutex
	sims map[string]*pdu.Simulated
	Now  func() time.Time
}

type rackSpec struct {
	name    string
	servers [][2]interface{} // outlet name, amps
}

var racks = []rackSpec{
	{"100", [][2]interface{}{{"100U31", 0.8}, {"100U29", 0.4}, {"100U24", 1.9}, {"100U22", 2.0}, {"N100U43", 0.3}, {"100U12", 1.6}, {"100U08", 1.2}, {"100U04", 0.5}}},
	{"200", [][2]interface{}{{"200U31", 0.5}, {"200U29", 0.9}, {"200U27", 1.0}, {"200U24", 0.9}, {"200U22", 1.2}, {"N200U43", 0.3}, {"N200U44", 0.4},
		{"200U20", 3.5}, {"200U18", 3.7}, {"200U16", 3.6}, {"200U13", 3.2}, {"200U12", 0.4}, {"200U03", 1.9}}},
	{"10F", [][2]interface{}{{"10FU1", 2.4}, {"10FU5", 2.2}, {"10FU9", 3.0}, {"10FU12", 4.0}, {"10FU14", 0.2}, {"10FU16", 3.9}, {"10FU19", 1.7}, {"10FU24", 1.1},
		{"10FU26", 0.9}, {"N10FU30", 0.9}, {"N10FU33", 0.9}, {"N10FU35", 0.5}}},
	{"20F", [][2]interface{}{{"20FU38", 4.5}, {"20FU33", 4.2}, {"20FU28", 4.0}, {"20FU22", 3.9}, {"20FU16", 3.7}, {"20FU10", 3.3}, {"20FU04", 4.4}}},
}

// hostLinks: hypervisor -> the outlet name of its power cable
var hostLinks = []struct{ host, outlet, rack string }{
	{"a41sf", "200U20", "200"}, {"a52sf", "200U18", "200"}, {"a47sf", "200U16", "200"}, {"a42sf", "200U13", "200"},
	{"h35sf", "200U31", "200"}, {"hspare2sf", "200U22", "200"}, {"a40sf", "100U24", "100"}, {"a39sf", "100U22", "100"},
	{"a44sf", "10FU12", "10F"}, {"a45sf", "10FU16", "10F"}, {"a50sf", "20FU38", "20F"}, {"a51sf", "20FU33", "20F"}, {"a53sf", "20FU28", "20F"},
	{"a54sf", "20FU22", "20F"}, {"jbof5sf", "", ""},
}

// Seed adds the sample data to an empty database. It does nothing when there are racks already.
func Seed(ctx context.Context, st *store.Store) (*Lab, error) {
	lab := &Lab{sims: map[string]*pdu.Simulated{}, Now: time.Now}
	existing, err := st.Racks(ctx)
	if err != nil {
		return nil, err
	}
	rackIDs := map[string]int64{}
	if len(existing) == 0 {
		for i, spec := range racks {
			r, err := st.SaveRack(ctx, store.Rack{Name: spec.name, MaxAmps: 24, Sort: i})
			if err != nil {
				return nil, err
			}
			rackIDs[spec.name] = r.ID
			for index, suffix := range []string{"A", "B"} {
				loads := map[int]pdu.SimOutlet{}
				for n, s := range spec.servers {
					loads[n+1] = pdu.SimOutlet{Name: s[0].(string), Amps: s[1].(float64) / 2}
				}
				vendor, model, count, metered := pdu.APC, "AP7932", 24, false
				if index == 1 || spec.name == "20F" {
					vendor, model, count, metered = pdu.CyberPower, "PDU81007", 16, true
					if len(loads) > 16 {
						return nil, fmt.Errorf("rack %s has too many servers for a 16 outlet PDU", spec.name)
					}
				}
				host := "demo-" + spec.name + suffix
				lab.sims[host] = pdu.NewSimulated(vendor, model, count, metered, loads)
				if _, err := st.SavePDU(ctx, store.PDU{RackID: r.ID, Name: "P" + spec.name + suffix, Vendor: vendor, Host: host, Port: 161, MaxAmps: 24},
					store.Communities{Read: "demo", Write: "demo"}, false); err != nil {
					return nil, err
				}
			}
		}
		for _, l := range hostLinks {
			h := store.Host{Name: l.host, Address: "demo-" + l.host, Port: 22, User: "hostinfo-ro", OutletKey: l.outlet, Enabled: true}
			if id, ok := rackIDs[l.rack]; ok {
				h.RackID = &id
			}
			saved, err := st.SaveHost(ctx, h)
			if err != nil {
				return nil, err
			}
			if err := st.TrustHostKey(ctx, saved.ID, "ssh-ed25519 DEMO"); err != nil {
				return nil, err
			}
		}
		return lab, nil
	}
	// a database that was seeded earlier: the simulated PDUs are rebuilt from the stored ones
	pdus, err := st.PDUs(ctx)
	if err != nil {
		return nil, err
	}
	for _, p := range pdus {
		for _, spec := range racks {
			if len(p.Name) > 1 && p.Name[1:len(p.Name)-1] == spec.name {
				loads := map[int]pdu.SimOutlet{}
				for n, s := range spec.servers {
					loads[n+1] = pdu.SimOutlet{Name: s[0].(string), Amps: s[1].(float64) / 2}
				}
				count, metered := 24, false
				if p.Vendor == pdu.CyberPower {
					count, metered = 16, true
				}
				lab.sims[p.Host] = pdu.NewSimulated(p.Vendor, "demo", count, metered, loads)
			}
		}
	}
	return lab, nil
}

// Factory is the driver factory of the demo: simulated PDUs by their address.
func (l *Lab) Factory(p store.PDU, _ store.Communities) (pdu.Driver, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if s, ok := l.sims[p.Host]; ok {
		return s, nil
	}
	return nil, fmt.Errorf("no demo PDU %q", p.Host)
}

// ---- hosts

var vmNames = []string{"altar22", "50webs-mail", "db-prod", "build-01", "files", "ldap-2", "monitor", "git", "wiki", "mx2", "backup-ctl", "vpn", "ci-runner", "dev-sandbox", "sip", "dns-int"}

func seed(name string) *fnvRand {
	h := fnv.New64a()
	h.Write([]byte(name))
	return &fnvRand{state: h.Sum64() | 1}
}

type fnvRand struct{ state uint64 }

func (r *fnvRand) next() float64 { // xorshift: the same host always has the same VMs
	r.state ^= r.state << 13
	r.state ^= r.state >> 7
	r.state ^= r.state << 17
	return float64(r.state%100000) / 100000
}

type demoVM struct {
	name string
	base float64
	rss  int64
}

func vmsOf(host string) (cores int, vms []demoVM, heavy bool) {
	r := seed(host)
	cores = []int{256, 128, 64}[int(r.next()*3)%3]
	n := 8 + int(r.next()*8)
	heavy = host == "a50sf" || host == "a53sf"
	off := int(r.next() * float64(len(vmNames))) // n is below len(vmNames), so the names of one host never repeat
	for i := 0; i < n; i++ {
		base := math.Pow(r.next(), 2) * float64(cores) * 0.05
		if heavy {
			base *= 3
		}
		vms = append(vms, demoVM{name: vmNames[(i+off)%len(vmNames)] + fmt.Sprintf("%d", (i+off)%3), base: base, rss: int64((1 + r.next()*11) * (1 << 30))})
	}
	return
}

// Runner answers for the sample hosts like the agent would.
func (l *Lab) Runner() hostinfo.Runner { return runner{l} }

type runner struct{ l *Lab }

func (r runner) Run(_ context.Context, h store.Host, command string) ([]byte, error) {
	now := r.l.Now()
	cores, vms, heavy := vmsOf(h.Name)
	wobble := func(i int, period float64) float64 { return 1 + 0.25*math.Sin(float64(now.Unix())/period+float64(i)) }
	switch command {
	case "status":
		s := hostinfo.Status{V: 1, Host: h.Name, T: now.Unix(), Interval: 60}
		used := 0.0
		s.VMs.Count = len(vms)
		for i, v := range vms {
			c := math.Round(v.base*wobble(i, 300)*100) / 100
			used += c
			s.VMs.List = append(s.VMs.List, hostinfo.VM{Name: v.name, PID: 4000 + i*13, RSS: v.rss, Cores: &c})
		}
		idle := math.Max(2, 100-100*(used+1.5)/float64(cores))
		idle = math.Round(idle*10) / 10
		io := 0.1
		s.CPU.Cores, s.CPU.IdlePct, s.CPU.IowaitPct = &cores, &idle, &io
		s.CPU.Load = []float64{math.Round(used*10) / 10, math.Round(used*9.6) / 10, math.Round(used*9.2) / 10}
		total := int64(cores) * 4 * (1 << 30)
		var vmMem int64
		for _, v := range vms {
			vmMem += v.rss
		}
		lvmHosts := map[string]bool{"a41sf": true, "a45sf": true, "a47sf": true} // hosts with LVM instead of ZFS, like a real LVM hypervisor
		arc := int64(80 << 30)
		if lvmHosts[h.Name] {
			arc = 0 // no ZFS, no ARC
		}
		s.Mem = &struct {
			Total     int64  `json:"total"`
			Available int64  `json:"available"`
			Used      int64  `json:"used"`
			SwapUsed  int64  `json:"swap_used"`
			ZfsArc    *int64 `json:"zfs_arc"`
		}{Total: total, Used: vmMem + arc + (20 << 30), SwapUsed: 0, ZfsArc: &arc}
		if lvmHosts[h.Name] {
			s.Mem.ZfsArc = nil
		}
		s.Mem.Available = total - s.Mem.Used
		switch {
		case lvmHosts[h.Name]:
			size := int64(22888000000000)
			free := int64(float64(size) * (0.22 + 0.1*seed(h.Name).next()))
			if h.Name == "a47sf" { // short on space: yellow
				free = size * 15 / 100
			}
			vg := hostinfo.VG{Name: "am", Size: size, Free: free, PVs: 1, LVs: len(vms) + 19}
			if h.Name == "a45sf" { // a thin pool that is nearly full: what the screen must show in colour
				one, two := 91.4, 12.0
				vg.Fill = []hostinfo.FillVolume{{Name: "thinpool", Kind: "thin-pool", Size: 4_000_000_000_000, DataPct: &one, MetaPct: &two}}
			}
			s.Storage = &hostinfo.Storage{T: now.Unix(), LVM: []hostinfo.VG{vg}}
		case h.Name != "jbof5sf":
			raw := int64(91534343012352)
			alloc := int64(float64(raw) * (0.55 + 0.1*seed(h.Name).next()))
			ut, ua := int64(float64(raw)*0.887), int64(float64(raw)*0.887)-int64(float64(alloc)*0.887)
			if h.Name == "a53sf" { // a pool that is nearly full: yellow
				ua = ut * 14 / 100
			}
			uu := ut - ua
			s.Storage = &hostinfo.Storage{T: now.Unix(), Zfs: true, Pools: []hostinfo.Pool{{Name: "pool", Size: raw, Alloc: alloc, Free: raw - alloc, CapPct: int(100 * alloc / raw), Health: "ONLINE",
				UsableUsed: &uu, UsableAvail: &ua, UsableTotal: &ut}}}
			if h.Name == "a54sf" {
				s.Storage.Pools[0].Health = "DEGRADED"
			}
		}
		if s.Storage != nil {
			const gb = int64(1) << 30
			s.Storage.FS = []hostinfo.Filesystem{{Mount: "/", Dev: "/dev/md126", Type: "ext4", Size: 46 * gb, Used: 6 * gb, Avail: 38 * gb},
				{Mount: "/boot", Dev: "/dev/md127", Type: "ext4", Size: 2 * gb, Used: gb / 4, Avail: 3 * gb / 2}}
			if lvmHosts[h.Name] {
				s.Storage.FS = append(s.Storage.FS, hostinfo.Filesystem{Mount: "/home/solusvm", Dev: "/dev/mapper/am-solusvm", Type: "ext4", Size: 118 * gb, Used: 69 * gb, Avail: 44 * gb})
			}
			if h.Name == "a50sf" { // a root filesystem that is nearly full
				s.Storage.FS[0].Used, s.Storage.FS[0].Avail = 44*gb, 1*gb
			}
		}
		other := 0.4
		s.OtherCores = &other
		s.Agent.Version, s.Agent.Python = "0.2.0", "3.9.25"
		s.Errors = []string{}
		_ = heavy
		return json.Marshal(s)
	case "top 1h", "top 6h", "top 24h", "top 7d":
		window := command[4:]
		seconds := map[string]float64{"1h": 3600, "6h": 21600, "24h": 86400, "7d": 604800}[window]
		t := hostinfo.Top{V: 1, Host: h.Name, Window: window, T: now.Unix(), Cores: cores, CoveredS: seconds, Coverage: 1, Samples: int(seconds / 60), OtherCPUS: seconds * 0.4}
		for i, v := range vms {
			avg := v.base * (1 + 0.3*math.Sin(float64(i)*1.7+float64(len(window))))
			peak := avg * (1.8 + 0.1*float64(len(window)))
			t.VMs = append(t.VMs, hostinfo.TopVM{Name: v.name, CPUS: math.Round(avg*seconds*10) / 10, AvgCores: math.Round(avg*100) / 100,
				AvgPct: math.Round(10000*avg/float64(cores)) / 100, PeakCores: math.Round(peak*100) / 100, PeakT: now.Unix() - int64(seconds/3*float64(i%3+1)/3)})
		}
		for i := 1; i < len(t.VMs); i++ {
			for j := i; j > 0 && t.VMs[j].CPUS > t.VMs[j-1].CPUS; j-- {
				t.VMs[j], t.VMs[j-1] = t.VMs[j-1], t.VMs[j]
			}
		}
		return json.Marshal(t)
	}
	return []byte(`{"error":"command not allowed"}`), nil
}

// Keys is the host key fetcher of the demo: every host "offers" the same fixed key.
type Keys struct{}

func (Keys) FetchHostKey(_ context.Context, _ string, _ int) (ssh.PublicKey, error) {
	key, _, _, _, err := ssh.ParseAuthorizedKey([]byte("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKNsWZl/ElZdOxUt0S5fwUmy2"))
	return key, err
}
