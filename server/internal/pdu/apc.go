package pdu

import (
	"context"

	"github.com/kblaskoff/pdu-monitor/server/internal/snmp"
)

// APC Rack PDUs (AP7932 and the like). Newer firmware speaks the rPDU2 MIB (1.3.6.1.4.1.318.1.1.26); older firmware only the original
// rPDU / sPDU MIBs (…318.1.1.12 and …318.1.1.4). The driver asks the device which one it has and remembers the answer.
type apc struct {
	t snmp.Transport
	memo
}

func (d *apc) Vendor() Vendor { return APC }

var (
	apcRoot = snmp.MustOID("1.3.6.1.4.1.318.1.1")

	// rPDU2
	r2Name     = []uint32{26, 2, 1, 3}
	r2Firm     = []uint32{26, 2, 1, 6}
	r2Model    = []uint32{26, 2, 1, 8}
	r2Serial   = []uint32{26, 2, 1, 9}
	r2Outlets  = []uint32{26, 4, 2, 1, 4}
	r2Phases   = []uint32{26, 4, 2, 1, 7}
	r2Banks    = []uint32{26, 4, 2, 1, 8}
	r2Orient   = []uint32{26, 4, 2, 1, 10}
	r2DevPower = []uint32{26, 4, 3, 1, 5} // hundredths of kW, -1 = unsupported
	r2PhCur    = []uint32{26, 6, 3, 1, 5}
	r2PhVolt   = []uint32{26, 6, 3, 1, 6}
	r2PhPower  = []uint32{26, 6, 3, 1, 7}
	r2BankCur  = []uint32{26, 8, 3, 1, 5}
	r2OName    = []uint32{26, 9, 2, 3, 1, 3}
	r2OState   = []uint32{26, 9, 2, 3, 1, 5} // 1 off, 2 on
	r2OBank    = []uint32{26, 9, 2, 2, 1, 6}
	r2OCommand = []uint32{26, 9, 2, 4, 1, 5} // 1 on, 2 off
	r2MCur     = []uint32{26, 9, 4, 3, 1, 6}
	r2MPower   = []uint32{26, 9, 4, 3, 1, 7}
	r2MName    = []uint32{26, 9, 4, 3, 1, 3}
	r2MBank    = []uint32{26, 9, 4, 2, 1, 7}

	// rPDU / sPDU (older firmware)
	lName     = []uint32{12, 1, 1}
	lFirm     = []uint32{12, 1, 3}
	lModel    = []uint32{12, 1, 5}
	lSerial   = []uint32{12, 1, 6}
	lOutlets  = []uint32{12, 1, 8}
	lPhases   = []uint32{12, 1, 9}
	lWatts    = []uint32{12, 1, 16}
	lLoad     = []uint32{12, 2, 3, 1, 1, 2}
	lLoadPh   = []uint32{12, 2, 3, 1, 1, 4}
	lLoadBank = []uint32{12, 2, 3, 1, 1, 5}
	sName     = []uint32{4, 4, 2, 1, 4}
	sCommand  = []uint32{4, 4, 2, 1, 3} // 1 on, 2 off
	lOBank    = []uint32{12, 3, 5, 1, 1, 6}
	lOLoad    = []uint32{12, 3, 5, 1, 1, 7}
)

func apcScalar(path []uint32) snmp.OID {
	return apcRoot.Append(append(append([]uint32{}, path...), 0)...)
}

func (d *apc) detect(ctx context.Context) (int, error) {
	d.mu.Lock()
	f := d.flavor
	d.mu.Unlock()
	if f != flavorUnknown {
		return f, nil
	}
	probe, err := snmp.GetAvailable(ctx, d.t, []snmp.OID{oid(apcRoot, r2Outlets, 1), apcScalar(lOutlets)}, 8)
	if err != nil {
		return 0, err
	}
	found := flavorUnknown
	if n, _ := probe.Int(oid(apcRoot, r2Outlets, 1)); n > 0 {
		found = flavorNew
	} else if n, _ := probe.Int(apcScalar(lOutlets)); n > 0 {
		found = flavorLegacy
	} else {
		return 0, &ErrNotRecognized{Vendor: "APC"}
	}
	d.mu.Lock()
	d.flavor = found
	d.mu.Unlock()
	return found, nil
}

func (d *apc) Poll(ctx context.Context) (*Snapshot, error) {
	f, err := d.detect(ctx)
	if err != nil {
		return nil, err
	}
	if f == flavorNew {
		return d.pollNew(ctx)
	}
	return d.pollLegacy(ctx)
}

func (d *apc) SetOutlet(ctx context.Context, n int, on bool) error {
	f, err := d.detect(ctx)
	if err != nil {
		return err
	}
	target := oid(apcRoot, r2OCommand, n)
	if f == flavorLegacy {
		target = oid(apcRoot, sCommand, n)
	}
	v := int64(2)
	if on {
		v = 1
	}
	return d.t.Set(ctx, []snmp.VarBind{{OID: target, Value: snmp.Integer(v)}})
}

func (d *apc) OutletState(ctx context.Context, n int) (*bool, error) {
	f, err := d.detect(ctx)
	if err != nil {
		return nil, err
	}
	if f == flavorNew {
		target := oid(apcRoot, r2OState, n)
		v, err := snmp.GetAvailable(ctx, d.t, []snmp.OID{target}, 1)
		if err != nil {
			return nil, err
		}
		s, ok := v.Int(target)
		if !ok {
			return nil, nil
		}
		return ptr(s == 2), nil // here 2 means on
	}
	target := oid(apcRoot, sCommand, n)
	v, err := snmp.GetAvailable(ctx, d.t, []snmp.OID{target}, 1)
	if err != nil {
		return nil, err
	}
	s, ok := v.Int(target)
	if !ok || (s != 1 && s != 2) {
		return nil, nil
	}
	return ptr(s == 1), nil
}

func (d *apc) pollNew(ctx context.Context) (*Snapshot, error) {
	head, err := snmp.GetAvailable(ctx, d.t, []snmp.OID{
		oid(apcRoot, r2Name, 1), oid(apcRoot, r2Firm, 1), oid(apcRoot, r2Model, 1), oid(apcRoot, r2Serial, 1),
		oid(apcRoot, r2Outlets, 1), oid(apcRoot, r2Phases, 1), oid(apcRoot, r2Banks, 1), oid(apcRoot, r2Orient, 1), oid(apcRoot, r2DevPower, 1),
	}, 8)
	if err != nil {
		return nil, err
	}
	outletCount, _ := head.Int(oid(apcRoot, r2Outlets, 1))
	if outletCount <= 0 {
		return nil, &ErrNotRecognized{Vendor: "APC"}
	}
	phaseCount, ok := head.Int(oid(apcRoot, r2Phases, 1))
	if !ok || phaseCount < 1 {
		phaseCount = 1
	}
	if phaseCount > 3 {
		phaseCount = 3
	}
	bankCount, _ := head.Int(oid(apcRoot, r2Banks, 1))
	d.mu.Lock()
	known := d.tables
	d.mu.Unlock()

	var want []snmp.OID
	for p := 1; p <= phaseCount; p++ {
		want = append(want, oid(apcRoot, r2PhCur, p), oid(apcRoot, r2PhVolt, p), oid(apcRoot, r2PhPower, p))
	}
	for b := 1; b <= bankCount; b++ {
		want = append(want, oid(apcRoot, r2BankCur, b))
	}
	for o := 1; o <= outletCount; o++ {
		if !known.known || known.switched {
			want = append(want, oid(apcRoot, r2OName, o), oid(apcRoot, r2OState, o), oid(apcRoot, r2OBank, o))
		}
		if !known.known || known.metered {
			want = append(want, oid(apcRoot, r2MName, o), oid(apcRoot, r2MBank, o), oid(apcRoot, r2MCur, o), oid(apcRoot, r2MPower, o))
		}
	}
	values, err := snmp.GetAvailable(ctx, d.t, want, 8)
	if err != nil {
		return nil, err
	}
	if !known.known {
		found := tableSet{known: true,
			switched: values.Has(oid(apcRoot, r2OState, 1)),
			metered:  values.Has(oid(apcRoot, r2MCur, 1)) || values.Has(oid(apcRoot, r2MPower, 1)) || values.Has(oid(apcRoot, r2MName, 1))}
		d.mu.Lock()
		d.tables = found
		d.mu.Unlock()
	}

	snap := &Snapshot{}
	for p := 1; p <= phaseCount; p++ {
		amps, ok := values.Tenths(oid(apcRoot, r2PhCur, p))
		if !ok {
			continue
		}
		ph := Phase{Number: p, Amps: amps}
		if n, ok := values.Int(oid(apcRoot, r2PhPower, p)); ok && n >= 0 {
			ph.Watts = ptr(float64(n) * 10) // hundredths of kW
		}
		if n, ok := values.Int(oid(apcRoot, r2PhVolt, p)); ok && n >= 0 {
			ph.Volts = ptr(float64(n))
		}
		snap.Phases = append(snap.Phases, ph)
	}
	for o := 1; o <= outletCount; o++ {
		name, _ := values.String(oid(apcRoot, r2OName, o))
		if name == "" {
			name, _ = values.String(oid(apcRoot, r2MName, o))
		}
		bank, bok := values.Int(oid(apcRoot, r2OBank, o))
		if !bok {
			bank, bok = values.Int(oid(apcRoot, r2MBank, o))
		}
		out := Outlet{Number: o, Name: cleanName(name), Bank: intPtr(bank, bok)}
		if s, ok := values.Int(oid(apcRoot, r2OState, o)); ok {
			out.On = ptr(s == 2)
		}
		amps, aok := values.Tenths(oid(apcRoot, r2MCur, o))
		out.Amps = asFloatPtr(amps, aok)
		out.Watts = wattsOf(values, oid(apcRoot, r2MPower, o))
		snap.Outlet = append(snap.Outlet, out)
	}
	for b := 1; b <= bankCount; b++ {
		if amps, ok := values.Tenths(oid(apcRoot, r2BankCur, b)); ok {
			snap.Banks = append(snap.Banks, Bank{Number: b, Amps: amps})
		}
	}
	info := Info{Vendor: APC, Outlets: outletCount}
	info.Model, _ = head.String(oid(apcRoot, r2Model, 1))
	info.Name, _ = head.String(oid(apcRoot, r2Name, 1))
	info.Serial, _ = head.String(oid(apcRoot, r2Serial, 1))
	info.Firmware, _ = head.String(oid(apcRoot, r2Firm, 1))
	if bankCount > 0 {
		info.Breakers = ptr(bankCount)
	}
	if o, ok := head.Int(oid(apcRoot, r2Orient, 1)); ok {
		info.Orientation = map[bool]string{true: "Horizontal", false: "Vertical"}[o == 1]
	}
	if len(snap.Phases) > 0 {
		info.LineVoltage = snap.Phases[0].Volts
	}
	snap.Info = info
	if n, ok := head.Int(oid(apcRoot, r2DevPower, 1)); ok && n >= 0 {
		snap.TotalWatts = ptr(float64(n) * 10)
	}
	snap.finish()
	return snap, nil
}

func (d *apc) pollLegacy(ctx context.Context) (*Snapshot, error) {
	head, err := snmp.GetAvailable(ctx, d.t, []snmp.OID{
		apcScalar(lName), apcScalar(lFirm), apcScalar(lModel), apcScalar(lSerial), apcScalar(lOutlets), apcScalar(lPhases), apcScalar(lWatts),
	}, 8)
	if err != nil {
		return nil, err
	}
	outletCount, _ := head.Int(apcScalar(lOutlets))
	if outletCount <= 0 {
		return nil, &ErrNotRecognized{Vendor: "APC"}
	}
	var want []snmp.OID
	for i := 1; i <= 8; i++ { // the load table lists the phases first and then the banks
		want = append(want, oid(apcRoot, lLoad, i), oid(apcRoot, lLoadPh, i), oid(apcRoot, lLoadBank, i))
	}
	for o := 1; o <= outletCount; o++ {
		want = append(want, oid(apcRoot, sName, o), oid(apcRoot, sCommand, o), oid(apcRoot, lOBank, o), oid(apcRoot, lOLoad, o))
	}
	values, err := snmp.GetAvailable(ctx, d.t, want, 8)
	if err != nil {
		return nil, err
	}
	snap := &Snapshot{}
	for i := 1; i <= 8; i++ {
		amps, ok := values.Tenths(oid(apcRoot, lLoad, i))
		if !ok {
			continue
		}
		bank, _ := values.Int(oid(apcRoot, lLoadBank, i))
		if bank > 0 {
			snap.Banks = append(snap.Banks, Bank{Number: bank, Amps: amps})
			continue
		}
		number, nok := values.Int(oid(apcRoot, lLoadPh, i))
		if !nok {
			number = len(snap.Phases) + 1
		}
		snap.Phases = append(snap.Phases, Phase{Number: number, Amps: amps})
	}
	for o := 1; o <= outletCount; o++ {
		name, _ := values.String(oid(apcRoot, sName, o))
		bank, bok := values.Int(oid(apcRoot, lOBank, o))
		out := Outlet{Number: o, Name: cleanName(name), Bank: intPtr(bank, bok)}
		if s, ok := values.Int(oid(apcRoot, sCommand, o)); ok && (s == 1 || s == 2) {
			out.On = ptr(s == 1)
		}
		amps, aok := values.Tenths(oid(apcRoot, lOLoad, o))
		out.Amps = asFloatPtr(amps, aok)
		snap.Outlet = append(snap.Outlet, out)
	}
	sortBanks(snap.Banks)
	info := Info{Vendor: APC, Outlets: outletCount}
	info.Model, _ = head.String(apcScalar(lModel))
	info.Name, _ = head.String(apcScalar(lName))
	info.Serial, _ = head.String(apcScalar(lSerial))
	info.Firmware, _ = head.String(apcScalar(lFirm))
	if len(snap.Banks) > 0 {
		info.Breakers = ptr(len(snap.Banks))
	}
	snap.Info = info
	if n, ok := head.Int(apcScalar(lWatts)); ok && n >= 0 {
		snap.TotalWatts = ptr(float64(n))
	}
	snap.finish()
	return snap, nil
}
