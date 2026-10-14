package pdu

import (
	"context"
	"fmt"
	"math"
	"sync"
)

// Simulated is a PDU that exists only in memory: for the demo mode and for tests. Outlets can be switched.
type Simulated struct {
	vendor  Vendor
	model   string
	count   int
	banks   int
	volts   float64
	metered bool
	mu      sync.Mutex
	names   map[int]string
	loads   map[int]float64
	states  map[int]bool
	tick    int
}

// SimOutlet is the name of an outlet and the amps it draws while on.
type SimOutlet struct {
	Name string
	Amps float64
}

func NewSimulated(v Vendor, model string, outlets int, metered bool, loads map[int]SimOutlet) *Simulated {
	s := &Simulated{vendor: v, model: model, count: outlets, banks: 2, volts: 230, metered: metered,
		names: map[int]string{}, loads: map[int]float64{}, states: map[int]bool{}}
	for n, o := range loads {
		s.names[n], s.loads[n] = o.Name, o.Amps
	}
	for n := 1; n <= outlets; n++ {
		s.states[n] = true
	}
	return s
}

func (s *Simulated) Vendor() Vendor { return s.vendor }

func (s *Simulated) Poll(ctx context.Context) (*Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tick++
	snap := &Snapshot{}
	truth := map[int]float64{} // what the outlet really draws, also when the PDU cannot measure it
	for n := 1; n <= s.count; n++ {
		on := s.states[n]
		base := 0.0
		if on {
			base = s.loads[n]
		}
		wobble := 0.0
		if base > 0 {
			wobble = math.Sin(float64(s.tick+n)/3) * 0.05 * base // a little movement so that the screens look alive
		}
		amps := math.Max(0, math.Round((base+wobble)*10)/10)
		truth[n] = amps
		name := s.names[n]
		if name == "" {
			name = fmt.Sprintf("Outlet_%d", n)
		}
		out := Outlet{Number: n, Name: name, Bank: ptr((n-1)*s.banks/s.count + 1), On: ptr(on)}
		if s.metered {
			out.Amps = ptr(amps)
			out.Watts = ptr(math.Round(amps * s.volts * 0.95))
		}
		snap.Outlet = append(snap.Outlet, out)
	}
	total := 0.0
	for b := 1; b <= s.banks; b++ {
		sum := 0.0
		for _, o := range snap.Outlet {
			if *o.Bank == b {
				sum += truth[o.Number]
			}
		}
		sum = math.Round(sum*10) / 10
		snap.Banks = append(snap.Banks, Bank{Number: b, Amps: sum})
		total += sum
	}
	watts := math.Round(total * s.volts * 0.95)
	orientation := "Horizontal"
	if s.count > 16 {
		orientation = "Vertical"
	}
	snap.Info = Info{Vendor: s.vendor, Model: s.model, Outlets: s.count, Breakers: ptr(s.banks), Orientation: orientation, LineVoltage: ptr(s.volts)}
	snap.Phases = []Phase{{Number: 1, Amps: total, Watts: ptr(watts), Volts: ptr(s.volts)}}
	snap.TotalAmps, snap.TotalWatts = ptr(total), ptr(watts)
	snap.finish()
	return snap, nil
}

func (s *Simulated) SetOutlet(ctx context.Context, n int, on bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.states[n]; !ok {
		return fmt.Errorf("no outlet %d", n)
	}
	s.states[n] = on
	return nil
}

// SetOutletName gives an outlet a name, like the real ones do.
func (s *Simulated) SetOutletName(ctx context.Context, n int, name string) error {
	if err := ValidOutletName(name); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.states[n]; !ok {
		return fmt.Errorf("no outlet %d", n)
	}
	s.names[n] = name
	return nil
}

func (s *Simulated) OutletState(ctx context.Context, n int) (*bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.states[n]; ok {
		return &v, nil
	}
	return nil, nil
}
