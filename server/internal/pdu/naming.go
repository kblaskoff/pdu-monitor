package pdu

import (
	"context"
	"errors"
	"fmt"

	"github.com/kblaskoff/pdu-monitor/server/internal/snmp"
)

// MaxOutletNameLen is the longest name that is written: the shortest limit of the supported devices (APC: 20 characters).
const MaxOutletNameLen = 20

// Namer is a PDU whose outlets can be given a name over SNMP.
type Namer interface {
	SetOutletName(ctx context.Context, number int, name string) error
}

// ErrNoNaming: this model or firmware has no object that this program knows for writing the name of an outlet.
var ErrNoNaming = errors.New("the names of the outlets of this PDU cannot be changed from here (older firmware)")

// ValidOutletName: printable ASCII, 1 to MaxOutletNameLen characters. What a PDU would do with anything else differs from model to model.
func ValidOutletName(name string) error {
	if name == "" || len(name) > MaxOutletNameLen {
		return fmt.Errorf("an outlet name has 1 to %d characters", MaxOutletNameLen)
	}
	for _, r := range name {
		if r < 0x20 || r > 0x7e {
			return errors.New("an outlet name may only use printable ASCII characters")
		}
	}
	return nil
}

// The objects that hold the name of an outlet and can be written are in the *Config* tables, not in the *Status* tables that are read
// (those are read-only): CPS-MIB ePDU2OutletSwitchedConfigName / MeteredConfigName / MonitoredConfigName, PowerNet-MIB
// rPDU2OutletSwitchedConfigName / rPDU2OutletMeteredConfigName and, on older firmware, rPDUOutletConfigOutletName.
var (
	cpsOCfgName   = []uint32{6, 1, 2, 1, 4}
	cpsMCfgName   = []uint32{6, 2, 2, 1, 4}
	cpsMonCfgName = []uint32{6, 3, 2, 1, 4}
	r2OCfgName    = []uint32{26, 9, 2, 1, 1, 3}
	r2MCfgName    = []uint32{26, 9, 4, 1, 1, 3}
	lOCfgName     = []uint32{12, 3, 4, 1, 1, 2}
)

func (d *cyberPower) SetOutletName(ctx context.Context, n int, name string) error {
	if err := ValidOutletName(name); err != nil {
		return err
	}
	f, err := d.detect(ctx)
	if err != nil {
		return err
	}
	if f == flavorLegacy {
		return ErrNoNaming
	}
	d.mu.Lock()
	known := d.tables
	d.mu.Unlock()
	if !known.known { // which tables the device has is learned by a reading
		if _, err := d.pollNew(ctx); err != nil {
			return err
		}
		d.mu.Lock()
		known = d.tables
		d.mu.Unlock()
	}
	var path []uint32
	switch { // the same order in which the name is read
	case known.switched:
		path = cpsOCfgName
	case known.metered:
		path = cpsMCfgName
	case known.monit:
		path = cpsMonCfgName
	default:
		return ErrNoNaming
	}
	return d.t.Set(ctx, []snmp.VarBind{{OID: oid(cpsRoot, path, n), Value: snmp.OctetString(name)}})
}

func (d *apc) SetOutletName(ctx context.Context, n int, name string) error {
	if err := ValidOutletName(name); err != nil {
		return err
	}
	f, err := d.detect(ctx)
	if err != nil {
		return err
	}
	var target snmp.OID
	if f == flavorLegacy {
		target = oid(apcRoot, lOCfgName, n)
	} else {
		d.mu.Lock()
		known := d.tables
		d.mu.Unlock()
		if !known.known {
			if _, err := d.pollNew(ctx); err != nil {
				return err
			}
			d.mu.Lock()
			known = d.tables
			d.mu.Unlock()
		}
		switch {
		case known.switched:
			target = oid(apcRoot, r2OCfgName, n)
		case known.metered:
			target = oid(apcRoot, r2MCfgName, n)
		default:
			return ErrNoNaming
		}
	}
	return d.t.Set(ctx, []snmp.VarBind{{OID: target, Value: snmp.OctetString(name)}})
}
