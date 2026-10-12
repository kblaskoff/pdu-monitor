// Package hostinfo reads the state of the hypervisors from the hostinfo agent (see hostagent/FORMAT.md) over SSH.
package hostinfo

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Status is what `hostinfo serve status` answers. Everything in it is untrusted text: a compromised host can write anything.
type Status struct {
	V        int     `json:"v"`
	Host     string  `json:"host"`
	T        int64   `json:"t"`
	Interval float64 `json:"interval"`
	CPU      struct {
		Cores     *int      `json:"cores"`
		IdlePct   *float64  `json:"idle_pct"`
		IowaitPct *float64  `json:"iowait_pct"`
		Load      []float64 `json:"load"`
	} `json:"cpu"`
	Mem *struct {
		Total     int64  `json:"total"`
		Available int64  `json:"available"`
		Used      int64  `json:"used"`
		SwapUsed  int64  `json:"swap_used"`
		ZfsArc    *int64 `json:"zfs_arc"`
	} `json:"mem"`
	Storage *struct {
		T     int64  `json:"t"`
		Zfs   bool   `json:"zfs"`
		Pools []Pool `json:"pools"`
	} `json:"storage"`
	VMs struct {
		Count int  `json:"count"`
		List  []VM `json:"list"`
	} `json:"vms"`
	OtherCores *float64 `json:"other_cores"`
	Agent      struct {
		Version string `json:"version"`
		Python  string `json:"python"`
	} `json:"agent"`
	Errors []string `json:"errors"`
}

// Pool is a ZFS pool: size/alloc/free are raw (they include the parity of raidz), usable_* is what the datasets can really hold.
type Pool struct {
	Name        string `json:"name"`
	Size        int64  `json:"size"`
	Alloc       int64  `json:"alloc"`
	Free        int64  `json:"free"`
	CapPct      int    `json:"cap_pct"`
	Health      string `json:"health"`
	UsableUsed  *int64 `json:"usable_used,omitempty"`
	UsableAvail *int64 `json:"usable_avail,omitempty"`
	UsableTotal *int64 `json:"usable_total,omitempty"`
}

// VM is one running virtual machine; Cores is the CPU it used since the previous sample (1.25 = a core and a quarter).
type VM struct {
	Name  string   `json:"name"`
	PID   int      `json:"pid"`
	RSS   int64    `json:"rss"`
	Cores *float64 `json:"cores"`
}

// Top is what `hostinfo serve top <window>` answers.
type Top struct {
	V         int     `json:"v"`
	Host      string  `json:"host"`
	Window    string  `json:"window"`
	T         int64   `json:"t"`
	Cores     int     `json:"cores"`
	CoveredS  float64 `json:"covered_s"`
	Coverage  float64 `json:"coverage"`
	Samples   int     `json:"samples"`
	OtherCPUS float64 `json:"other_cpu_s"`
	VMs       []TopVM `json:"vms"`
}

type TopVM struct {
	Name      string  `json:"name"`
	CPUS      float64 `json:"cpu_s"`
	AvgCores  float64 `json:"avg_cores"`
	AvgPct    float64 `json:"avg_pct"`
	PeakCores float64 `json:"peak_cores"`
	PeakT     int64   `json:"peak_t"`
}

// MaxAnswer is the biggest answer that is read (a status is a few kB, a 7 day top under 20 kB).
const MaxAnswer = 256 * 1024

// Windows are the periods `top` answers for.
var Windows = []string{"1h", "6h", "24h", "7d"}

func ValidWindow(w string) bool {
	for _, v := range Windows {
		if v == w {
			return true
		}
	}
	return false
}

func clean(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= ' ' && r != 0x7f {
			b.WriteRune(r)
		}
	}
	out := strings.TrimSpace(b.String())
	if r := []rune(out); len(r) > 100 {
		out = string(r[:100])
	}
	return out
}

func decode(data []byte, v any) error {
	if len(data) > MaxAnswer {
		return errors.New("the answer is too big")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("the answer is not valid: %w", err)
	}
	return nil
}

// ParseStatus decodes and cleans a status answer.
func ParseStatus(data []byte) (*Status, error) {
	var raw struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(data, &raw)
	if raw.Error != "" {
		return nil, fmt.Errorf("the host says: %s", clean(raw.Error))
	}
	var s Status
	if err := decode(data, &s); err != nil {
		return nil, err
	}
	if s.V != 1 {
		return nil, fmt.Errorf("unknown format version %d", s.V)
	}
	s.Host = clean(s.Host)
	if len(s.VMs.List) > 500 {
		s.VMs.List = s.VMs.List[:500]
	}
	for i := range s.VMs.List {
		s.VMs.List[i].Name = clean(s.VMs.List[i].Name)
	}
	if s.Storage != nil {
		if len(s.Storage.Pools) > 50 {
			s.Storage.Pools = s.Storage.Pools[:50]
		}
		for i := range s.Storage.Pools {
			s.Storage.Pools[i].Name = clean(s.Storage.Pools[i].Name)
			s.Storage.Pools[i].Health = clean(s.Storage.Pools[i].Health)
		}
	}
	if len(s.Errors) > 20 {
		s.Errors = s.Errors[:20]
	}
	for i := range s.Errors {
		s.Errors[i] = clean(s.Errors[i])
	}
	if len(s.CPU.Load) > 3 {
		s.CPU.Load = s.CPU.Load[:3]
	}
	s.Agent.Version, s.Agent.Python = clean(s.Agent.Version), clean(s.Agent.Python)
	return &s, nil
}

// ParseTop decodes and cleans a top answer.
func ParseTop(data []byte, window string) (*Top, error) {
	var raw struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(data, &raw)
	if raw.Error != "" {
		return nil, fmt.Errorf("the host says: %s", clean(raw.Error))
	}
	var t Top
	if err := decode(data, &t); err != nil {
		return nil, err
	}
	if t.V != 1 || t.Window != window {
		return nil, errors.New("the answer is for another question")
	}
	t.Host = clean(t.Host)
	if len(t.VMs) > 500 {
		t.VMs = t.VMs[:500]
	}
	for i := range t.VMs {
		t.VMs[i].Name = clean(t.VMs[i].Name)
	}
	return &t, nil
}

// Age is how old a sample is.
func (s *Status) Age(now time.Time) time.Duration {
	return now.Sub(time.Unix(s.T, 0))
}
