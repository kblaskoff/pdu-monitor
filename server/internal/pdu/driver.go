package pdu

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/kblaskoff/pdu-monitor/server/internal/snmp"
)

// Driver is one PDU: read it, switch an outlet, read the state of an outlet.
type Driver interface {
	Vendor() Vendor
	Poll(ctx context.Context) (*Snapshot, error)
	SetOutlet(ctx context.Context, number int, on bool) error
	// OutletState is nil when the PDU cannot tell.
	OutletState(ctx context.Context, number int) (*bool, error)
}

// NewDriver makes the driver for a vendor on top of an SNMP transport.
func NewDriver(v Vendor, t snmp.Transport) (Driver, error) {
	switch v {
	case CyberPower:
		return &cyberPower{t: t}, nil
	case APC:
		return &apc{t: t}, nil
	}
	return nil, fmt.Errorf("unknown vendor %q", v)
}

// ErrNotRecognized: the device does not answer like a PDU of the chosen make.
type ErrNotRecognized struct{ Vendor string }

func (e *ErrNotRecognized) Error() string {
	return fmt.Sprintf("The device does not answer like a %s PDU (wrong vendor, community or SNMP disabled?)", e.Vendor)
}

var errNoSwitching = errors.New("this PDU cannot switch outlets (it is a metered or monitored model)")

// oid builds root + path + index.
func oid(root snmp.OID, path []uint32, index int) snmp.OID {
	o := root.Append(path...)
	return o.Append(uint32(index))
}

func asFloatPtr(v float64, ok bool) *float64 {
	if !ok {
		return nil
	}
	return &v
}

func intPtr(n int, ok bool) *int {
	if !ok {
		return nil
	}
	return &n
}

// watts of a value given in watts; negative means "not supported" on some APC objects.
func wattsOf(v snmp.Values, o snmp.OID) *float64 {
	n, ok := v.Int(o)
	if !ok || n < 0 {
		return nil
	}
	return ptr(float64(n))
}

type tableSet struct {
	known                    bool
	switched, metered, monit bool
}

// once-detected facts about a device, shared by the polls
type memo struct {
	mu     sync.Mutex
	flavor int
	tables tableSet
}
