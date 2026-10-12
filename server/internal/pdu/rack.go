package pdu

import (
	"sort"
	"strings"
	"unicode"
)

// State is a PDU of a rack together with its last reading.
type State struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	Vendor      Vendor    `json:"vendor"`
	MaxAmps     float64   `json:"maxAmps"`
	BankMaxAmps *float64  `json:"bankMaxAmps"`
	Snapshot    *Snapshot `json:"snapshot"`
	Error       string    `json:"error,omitempty"`
}

// Amps/Watts of the whole PDU (nil until it has been read).
func (s State) Amps() *float64 {
	if s.Snapshot == nil {
		return nil
	}
	return s.Snapshot.TotalAmps
}

func (s State) Watts() *float64 {
	if s.Snapshot == nil {
		return nil
	}
	return s.Snapshot.TotalWatts
}

func (s State) Status(warn float64) Status {
	limit := s.MaxAmps
	return Evaluate(s.Amps(), &limit, warn)
}

// Target is one physical port: which PDU, which outlet.
type Target struct {
	PDUID  int64 `json:"pdu"`
	Outlet int   `json:"outlet"`
}

// Port is a port of a server with what the PDU reports about it.
type Port struct {
	Target
	PDUName string `json:"pduName"`
	Reading Outlet `json:"reading"`
}

// Server is one server (or switch) of a rack: the outlets named the same on the PDUs of the rack, put together.
type Server struct {
	Key   string `json:"key"`
	Name  string `json:"name"`
	Kind  string `json:"kind"`
	Ports []Port `json:"ports"`
}

// Watts is nil when no port of the server is metered (some APC models).
func (s Server) Watts() *float64 {
	sum, any := 0.0, false
	for _, p := range s.Ports {
		if p.Reading.Watts != nil {
			sum += *p.Reading.Watts
			any = true
		}
	}
	if !any {
		return nil
	}
	return &sum
}

func (s Server) Amps() *float64 {
	sum, any := 0.0, false
	for _, p := range s.Ports {
		if p.Reading.Amps != nil {
			sum += *p.Reading.Amps
			any = true
		}
	}
	if !any {
		return nil
	}
	return &sum
}

// Power is "on", "off", "mixed" (some ports on, some off) or "unknown".
func (s Server) Power() string {
	on, off, unknown := 0, 0, 0
	for _, p := range s.Ports {
		switch {
		case p.Reading.On == nil:
			unknown++
		case *p.Reading.On:
			on++
		default:
			off++
		}
	}
	switch {
	case unknown == len(s.Ports):
		return "unknown"
	case unknown > 0:
		return "mixed"
	case off == 0:
		return "on"
	case on == 0:
		return "off"
	}
	return "mixed"
}

func (s Server) Targets() []Target {
	out := make([]Target, len(s.Ports))
	for i, p := range s.Ports {
		out[i] = p.Target
	}
	return out
}

// Servers puts the outlets with the same name together. Outlets with the PDU's default name are not servers.
func Servers(pdus []State) []Server {
	index := map[string]int{}
	var servers []Server
	for _, p := range pdus {
		if p.Snapshot == nil {
			continue
		}
		for _, o := range p.Snapshot.Outlet {
			if !o.Assigned() {
				continue
			}
			key := ServerKey(o.Name)
			port := Port{Target: Target{PDUID: p.ID, Outlet: o.Number}, PDUName: p.Name, Reading: o}
			if i, ok := index[key]; ok {
				servers[i].Ports = append(servers[i].Ports, port)
				continue
			}
			name := strings.TrimSpace(o.Name)
			index[key] = len(servers)
			servers = append(servers, Server{Key: key, Name: name, Kind: ServerKind(name), Ports: []Port{port}})
		}
	}
	sort.SliceStable(servers, func(i, j int) bool { return naturalLess(servers[i].Name, servers[j].Name) })
	return servers
}

// naturalLess sorts "200U9" before "200U31", ignoring case.
func naturalLess(a, b string) bool {
	a, b = strings.ToLower(a), strings.ToLower(b)
	ra, rb := []rune(a), []rune(b)
	i, j := 0, 0
	for i < len(ra) && j < len(rb) {
		if unicode.IsDigit(ra[i]) && unicode.IsDigit(rb[j]) {
			si := i
			for i < len(ra) && unicode.IsDigit(ra[i]) {
				i++
			}
			sj := j
			for j < len(rb) && unicode.IsDigit(rb[j]) {
				j++
			}
			na := strings.TrimLeft(string(ra[si:i]), "0")
			nb := strings.TrimLeft(string(rb[sj:j]), "0")
			if len(na) != len(nb) {
				return len(na) < len(nb)
			}
			if na != nb {
				return na < nb
			}
			continue
		}
		if ra[i] != rb[j] {
			return ra[i] < rb[j]
		}
		i++
		j++
	}
	return len(ra)-i < len(rb)-j
}

// Summary is what a rack tile shows.
type Summary struct {
	Amps     *float64 `json:"amps"`
	Watts    *float64 `json:"watts"`
	Limit    float64  `json:"limit"`
	Status   Status   `json:"status"`
	WorstPDU Status   `json:"worstPdu"`
	// Attention is as bad as the rack itself or the worst PDU in it: either limit counts.
	Attention Status   `json:"attention"`
	Fraction  *float64 `json:"fraction"`
	PDUs      int      `json:"pdus"`
	Offline   int      `json:"offline"`
}

// Summarize adds up the PDUs of a rack and compares the total with the limit of the rack and each PDU with its own limit.
func Summarize(limit float64, pdus []State, warn float64) Summary {
	var amps, watts float64
	haveAmps, haveWatts := false, false
	worst := StatusUnknown
	offline := 0
	for _, p := range pdus {
		if a := p.Amps(); a != nil {
			amps += *a
			haveAmps = true
		}
		if w := p.Watts(); w != nil {
			watts += *w
			haveWatts = true
		}
		if p.Snapshot == nil || p.Error != "" {
			offline++
		}
		if st := p.Status(warn); st != StatusUnknown && (worst == StatusUnknown || st > worst) {
			worst = st
		}
	}
	s := Summary{Limit: limit, PDUs: len(pdus), Offline: offline, WorstPDU: worst}
	if haveAmps {
		s.Amps = &amps
		if limit > 0 {
			f := amps / limit
			s.Fraction = &f
		}
	}
	if haveWatts {
		s.Watts = &watts
	}
	s.Status = Evaluate(s.Amps, &limit, warn)
	s.Attention = s.Status
	if worst != StatusUnknown && worst > s.Attention {
		s.Attention = worst
	}
	return s
}
