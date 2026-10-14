package servman

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/kblaskoff/pdu-monitor/server/internal/pdu"
)

// Name formats: what an outlet is called.
const (
	FormatCCID     = "ccid"      // 200U13
	FormatCCIDName = "ccid_name" // 200U13 a42sf
)

// PDUInfo is a PDU of this system as the plan needs it: its name (what servman calls it), the names its outlets have now and whether it can be written to.
type PDUInfo struct {
	ID       int64
	Name     string
	Read     bool           // false until the PDU was read once: its outlets are not known
	Outlets  map[int]string // number -> current name (the PDU's own default names included)
	HasWrite bool           // a write community is configured
	Namer    bool           // the driver can write names
}

// Change is one outlet whose name differs from what servman says.
type Change struct {
	PDUID   int64  `json:"pduId"`
	PDU     string `json:"pdu"`
	Outlet  int    `json:"outlet"`
	Current string `json:"current"`
	Desired string `json:"desired"`
	Device  string `json:"device"` // the ccid
	Host    string `json:"host,omitempty"`
	// Kind is "fill" when the outlet still has the PDU's default name, "rename" when another name is replaced.
	Kind string `json:"kind"`
	// Blocked says why it cannot be written (no write community, the model cannot be renamed); the change is shown but not applied.
	Blocked string `json:"blocked,omitempty"`
	Note    string `json:"note,omitempty"`
}

// Warning is something in servman or on a PDU that does not fit; nothing is written for it.
type Warning struct {
	Kind   string `json:"kind"` // unknown-pdu, conflict, out-of-range, bad-name, not-read
	PDU    string `json:"pdu,omitempty"`
	Outlet int    `json:"outlet,omitempty"`
	Text   string `json:"text"`
}

// Unmapped is an outlet that has a name of its own on the PDU but is not in servman at all: shown, never touched.
type Unmapped struct {
	PDU    string `json:"pdu"`
	Outlet int    `json:"outlet"`
	Name   string `json:"name"`
}

// Plan is what a comparison of servman and the PDUs found.
type Plan struct {
	Time      time.Time  `json:"time"`
	Format    string     `json:"format"`
	Devices   int        `json:"devices"`   // devices of servman with at least one PDU port
	Unchanged int        `json:"unchanged"` // outlets that already have the right name
	Changes   []Change   `json:"changes"`
	Warnings  []Warning  `json:"warnings"`
	Unmapped  []Unmapped `json:"unmapped"`
}

type slot struct {
	pdu    string
	outlet int
}

type claim struct {
	dev      Device
	conflict bool
}

// OutletName is the name an outlet gets for a device in a format. The second result says that the machine name had to be cut (or left out) to fit.
func OutletName(d Device, format string) (name string, shortened bool, err error) {
	ccid := strings.TrimSpace(d.CCID)
	name = ccid
	if format == FormatCCIDName {
		host := safeHost(d.Name)
		if host != "" && !strings.EqualFold(host, ccid) {
			full := ccid + " " + host
			if len(full) > pdu.MaxOutletNameLen {
				shortened = true
				if room := pdu.MaxOutletNameLen - len(ccid) - 1; room >= 4 { // a piece of the machine name shorter than this says nothing
					full = ccid + " " + host[:room]
				} else {
					full = ccid
				}
			}
			name = full
		}
	}
	if err := pdu.ValidOutletName(name); err != nil {
		return "", false, fmt.Errorf("%q: %v", name, err)
	}
	return name, shortened, nil
}

// safeHost keeps the characters of a host name; anything else (spaces, non-ASCII letters) is dropped.
func safeHost(s string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(s) {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Build compares servman with the PDUs. It writes nothing. Rules that keep it safe:
//   - only outlets that servman names are ever changed; an outlet that servman does not know is only reported (Unmapped);
//   - when two devices claim the same outlet neither is applied (a conflict is a mistake in servman that a person must fix);
//   - a PDU that has not been read, or is not in servman's list, is reported, not guessed at.
func Build(devices []Device, pdus []PDUInfo, format string, now time.Time) Plan {
	if format != FormatCCIDName {
		format = FormatCCID
	}
	plan := Plan{Time: now, Format: format, Changes: []Change{}, Warnings: []Warning{}, Unmapped: []Unmapped{}}
	byName := map[string]PDUInfo{}
	for _, p := range pdus {
		byName[strings.ToLower(strings.TrimSpace(p.Name))] = p
	}

	claims := map[slot]*claim{}
	unknown := map[string]int{}
	for _, d := range devices {
		if len(d.Ports) == 0 {
			continue
		}
		plan.Devices++
		seen := map[slot]bool{}
		for _, port := range d.Ports {
			p, ok := byName[strings.ToLower(port.PDU)]
			if !ok {
				unknown[port.PDU]++
				continue
			}
			s := slot{p.Name, port.Port}
			if seen[s] {
				continue
			}
			seen[s] = true
			if c, taken := claims[s]; taken {
				if !c.conflict {
					plan.Warnings = append(plan.Warnings, Warning{Kind: "conflict", PDU: p.Name, Outlet: port.Port,
						Text: fmt.Sprintf("servman puts both %s and %s on %s outlet %d: nothing is written there", c.dev.CCID, d.CCID, p.Name, port.Port)})
				}
				c.conflict = true
				continue
			}
			claims[s] = &claim{dev: d}
		}
	}
	names := make([]string, 0, len(unknown))
	for n := range unknown {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		plan.Warnings = append(plan.Warnings, Warning{Kind: "unknown-pdu", PDU: n,
			Text: fmt.Sprintf("servman has %d cable(s) on %s, which is not a PDU of this system (add it, or it is named differently)", unknown[n], n)})
	}

	mapped := map[slot]bool{}
	for _, p := range pdus {
		var slots []slot
		for s := range claims {
			if s.pdu == p.Name {
				slots = append(slots, s)
			}
		}
		sort.Slice(slots, func(i, j int) bool { return slots[i].outlet < slots[j].outlet })
		if !p.Read {
			if len(slots) > 0 {
				plan.Warnings = append(plan.Warnings, Warning{Kind: "not-read", PDU: p.Name, Text: p.Name + " has not been read yet (or does not answer): its outlets are not compared"})
			}
			continue
		}
		for _, s := range slots {
			c := claims[s]
			mapped[s] = true
			if c.conflict {
				continue
			}
			current, exists := p.Outlets[s.outlet]
			if !exists {
				plan.Warnings = append(plan.Warnings, Warning{Kind: "out-of-range", PDU: p.Name, Outlet: s.outlet,
					Text: fmt.Sprintf("servman puts %s on %s outlet %d, but this PDU has no such outlet", c.dev.CCID, p.Name, s.outlet)})
				continue
			}
			desired, shortened, err := OutletName(c.dev, format)
			if err != nil {
				plan.Warnings = append(plan.Warnings, Warning{Kind: "bad-name", PDU: p.Name, Outlet: s.outlet, Text: "cannot be used as an outlet name: " + err.Error()})
				continue
			}
			if strings.TrimSpace(current) == desired {
				plan.Unchanged++
				continue
			}
			ch := Change{PDUID: p.ID, PDU: p.Name, Outlet: s.outlet, Current: current, Desired: desired, Device: c.dev.CCID, Host: c.dev.Name, Kind: "rename"}
			if pdu.IsDefaultOutletName(current) {
				ch.Kind = "fill"
			}
			if shortened {
				ch.Note = "the machine name was shortened to fit"
			}
			switch {
			case !p.HasWrite:
				ch.Blocked = "no write community is set for this PDU"
			case !p.Namer:
				ch.Blocked = "the driver of this PDU cannot rename outlets"
			}
			plan.Changes = append(plan.Changes, ch)
		}
		var numbers []int
		for n := range p.Outlets {
			numbers = append(numbers, n)
		}
		sort.Ints(numbers)
		for _, n := range numbers {
			if !mapped[slot{p.Name, n}] && !pdu.IsDefaultOutletName(p.Outlets[n]) {
				plan.Unmapped = append(plan.Unmapped, Unmapped{PDU: p.Name, Outlet: n, Name: p.Outlets[n]})
			}
		}
	}
	sort.SliceStable(plan.Changes, func(i, j int) bool {
		a, b := plan.Changes[i], plan.Changes[j]
		if a.PDU != b.PDU {
			return a.PDU < b.PDU
		}
		return a.Outlet < b.Outlet
	})
	return plan
}
