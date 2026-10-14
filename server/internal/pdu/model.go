// Package pdu reads and switches rack PDUs (CyberPower and APC) over SNMPv1 and does the rack arithmetic on what it reads.
package pdu

import (
	"regexp"
	"strings"
	"time"
)

// Vendor is the make of a PDU.
type Vendor string

const (
	CyberPower Vendor = "cyberpower"
	APC        Vendor = "apc"
)

func (v Vendor) Display() string {
	switch v {
	case CyberPower:
		return "CyberPower"
	case APC:
		return "APC"
	}
	return string(v)
}

func (v Vendor) Valid() bool { return v == CyberPower || v == APC }

func ptr[T any](v T) *T { return &v }

// Outlet is one outlet as read from a PDU. Values the PDU cannot measure are nil (not 0), so a screen can show "—".
type Outlet struct {
	Number int      `json:"number"`
	Name   string   `json:"name"`
	Bank   *int     `json:"bank"`
	On     *bool    `json:"on"`
	Amps   *float64 `json:"amps"`
	Watts  *float64 `json:"watts"`
}

// Assigned is false for the PDU's default names ("Outlet_7", empty ...): nothing is plugged into the outlet by name.
func (o Outlet) Assigned() bool { return !IsDefaultOutletName(o.Name) }

type Bank struct {
	Number int      `json:"number"`
	Amps   float64  `json:"amps"`
	Watts  *float64 `json:"watts"`
}

type Phase struct {
	Number int      `json:"number"`
	Amps   float64  `json:"amps"`
	Watts  *float64 `json:"watts"`
	Volts  *float64 `json:"volts"`
}

type Info struct {
	Vendor      Vendor   `json:"vendor"`
	Model       string   `json:"model,omitempty"`
	Name        string   `json:"name,omitempty"`
	Serial      string   `json:"serial,omitempty"`
	Firmware    string   `json:"firmware,omitempty"`
	Outlets     int      `json:"outlets"`
	Breakers    *int     `json:"breakers"`
	Orientation string   `json:"orientation,omitempty"`
	LineVoltage *float64 `json:"lineVoltage"`
}

// Snapshot is everything one poll of one PDU returns.
type Snapshot struct {
	Time   time.Time `json:"time"`
	Info   Info      `json:"info"`
	Outlet []Outlet  `json:"outlets"`
	Banks  []Bank    `json:"banks"`
	Phases []Phase   `json:"phases"`
	// Totals of the whole PDU: from the device when it reports them, else from the phases, banks or outlets.
	TotalAmps  *float64 `json:"totalAmps"`
	TotalWatts *float64 `json:"totalWatts"`
}

// HasOutletMetering is true when the PDU measures single outlets (not only banks or the whole unit).
func (s *Snapshot) HasOutletMetering() bool {
	for _, o := range s.Outlet {
		if o.Amps != nil || o.Watts != nil {
			return true
		}
	}
	return false
}

// finish fills in the totals the device did not give.
func (s *Snapshot) finish() {
	if s.Time.IsZero() {
		s.Time = time.Now()
	}
	if s.TotalAmps == nil {
		switch {
		case len(s.Phases) > 1:
			// A three-phase PDU is limited per phase: the busiest phase is what is compared with the limit.
			m := s.Phases[0].Amps
			for _, p := range s.Phases[1:] {
				if p.Amps > m {
					m = p.Amps
				}
			}
			s.TotalAmps = ptr(m)
		case len(s.Phases) == 1:
			s.TotalAmps = ptr(s.Phases[0].Amps)
		case len(s.Banks) > 0:
			sum := 0.0
			for _, b := range s.Banks {
				sum += b.Amps
			}
			s.TotalAmps = ptr(sum)
		default:
			sum, any := 0.0, false
			for _, o := range s.Outlet {
				if o.Amps != nil {
					sum += *o.Amps
					any = true
				}
			}
			if any {
				s.TotalAmps = ptr(sum)
			}
		}
	}
	if s.TotalWatts == nil {
		sum, any := 0.0, false
		for _, p := range s.Phases {
			if p.Watts != nil {
				sum += *p.Watts
				any = true
			}
		}
		if !any {
			for _, o := range s.Outlet {
				if o.Watts != nil {
					sum += *o.Watts
					any = true
				}
			}
		}
		if any {
			s.TotalWatts = ptr(sum)
		}
	}
	// the wattage of a bank comes from its outlets when they are metered (the PDUs do not report it)
	for i := range s.Banks {
		if s.Banks[i].Watts != nil {
			continue
		}
		sum, any := 0.0, false
		for _, o := range s.Outlet {
			if o.Bank != nil && *o.Bank == s.Banks[i].Number && o.Watts != nil {
				sum += *o.Watts
				any = true
			}
		}
		if any {
			s.Banks[i].Watts = ptr(sum)
		}
	}
}

// IsDefaultOutletName: "Outlet_7", "Outlet 7", "outlet-07", "OUTLET7" and an empty name are the PDU's defaults.
func IsDefaultOutletName(name string) bool {
	t := strings.TrimSpace(name)
	if t == "" {
		return true
	}
	lower := strings.ToLower(t)
	if !strings.HasPrefix(lower, "outlet") {
		return false
	}
	rest := strings.TrimLeft(lower[len("outlet"):], "_ -")
	for _, r := range rest {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// ServerKey is what finds the outlets of one server on two PDUs: case-insensitive, without spaces.
func ServerKey(name string) string {
	if ccid, ok := ccidToken(name); ok {
		return strings.ToLower(ccid)
	}
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(name), " ", ""))
}

var ccidPattern = regexp.MustCompile(`^(?i)N?[0-9]{1,4}[A-Z]?U[0-9]{1,3}(-[0-9]+)?$`)

// ccidToken: an outlet can be named "200U13" or "200U13 a42sf" (the id of the rack unit, then the name of the machine). When the name
// starts with such an id followed by more words, the id alone is what identifies the server: both PDUs of one server must be one line
// whatever follows, and the host is joined by the id.
func ccidToken(name string) (string, bool) {
	fields := strings.Fields(name)
	if len(fields) > 1 && ccidPattern.MatchString(fields[0]) {
		return fields[0], true
	}
	return "", false
}

// ServerKind: network gear is named with an N in front of the rack unit: "N200U43", "N10FU30" (N, the rack, U, the unit).
func ServerKind(name string) string {
	if ccid, ok := ccidToken(name); ok {
		name = ccid
	}
	upper := strings.ToUpper(strings.TrimSpace(name))
	if !strings.HasPrefix(upper, "N") || len(upper) < 4 {
		return "server"
	}
	rest := upper[1:]
	if rest[0] < '0' || rest[0] > '9' {
		return "server"
	}
	u := strings.LastIndex(rest, "U")
	if u <= 0 {
		return "server"
	}
	for _, r := range rest[:u] {
		if !(r >= '0' && r <= '9') && !(r >= 'A' && r <= 'Z') {
			return "server"
		}
	}
	unit := rest[u+1:]
	if unit == "" || unit[0] < '0' || unit[0] > '9' {
		return "server"
	}
	return "network"
}

// cleanName removes control characters and cuts the length: the names come from the devices, so they are untrusted text.
func cleanName(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= ' ' && r != 0x7f {
			b.WriteRune(r)
		}
	}
	out := b.String()
	if len([]rune(out)) > 100 {
		out = string([]rune(out)[:100])
	}
	return strings.TrimSpace(out)
}

// Status says where a power limit stands: for racks, PDUs and banks.
type Status int

const (
	StatusOK Status = iota
	StatusWarning
	StatusOver
	StatusUnknown
)

func (s Status) String() string {
	return [...]string{"ok", "warning", "over", "unknown"}[s]
}

func (s Status) MarshalText() ([]byte, error) { return []byte(s.String()), nil }

// Evaluate compares the amps with the limit; warn is the fraction where the warning starts (0.8 = 80 %).
func Evaluate(amps *float64, limit *float64, warn float64) Status {
	if amps == nil || limit == nil || *limit <= 0 {
		return StatusUnknown
	}
	// The readings have one decimal: the tolerance keeps 19.2 A of 24 A at 80 % from falling below the line by rounding.
	const eps = 1e-9
	switch {
	case *amps > *limit+eps:
		return StatusOver
	case *amps+eps >= *limit*warn:
		return StatusWarning
	}
	return StatusOK
}
