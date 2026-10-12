package pdu

import (
	"context"

	"github.com/kblaskoff/pdu-monitor/server/internal/snmp"
)

// CyberPower PDUs. Two generations are understood, the driver asks the device which one it has and remembers it:
//   - ePDU2 (CPS-MIB 1.3.6.1.4.1.3808.1.1.6): PDU81xxx, PDU41xxx, PDU31xxx, PDU23xxx and the other current models: switched and/or
//     metered by outlet, monitored (names only), metered by unit/bank, 1 or 3 phases.
//   - ePDU (1.3.6.1.4.1.3808.1.1.3): the older "SW" models (PDU15SW8FNET, PDU20SWHVIEC8FNET ...).
type cyberPower struct {
	t snmp.Transport
	memo
}

func (d *cyberPower) Vendor() Vendor { return CyberPower }

var (
	cpsRoot    = snmp.MustOID("1.3.6.1.4.1.3808.1.1.6")
	cpsLegacy  = snmp.MustOID("1.3.6.1.4.1.3808.1.1.3")
	cpsModel   = []uint32{2, 2, 1, 9}
	cpsName    = []uint32{2, 2, 1, 3}
	cpsSerial  = []uint32{2, 2, 1, 10}
	cpsFirm    = []uint32{2, 2, 1, 7}
	cpsOutlets = []uint32{3, 3, 1, 5}
	cpsPhases  = []uint32{3, 3, 1, 8}
	cpsBreaker = []uint32{3, 3, 1, 9}
	cpsOrient  = []uint32{3, 3, 1, 11}
	cpsDevAmps = []uint32{3, 4, 1, 5}  // tenths of amps
	cpsDevWatt = []uint32{3, 4, 1, 18} // watts
	cpsPhLoad  = []uint32{4, 4, 1, 5}
	cpsPhVolt  = []uint32{4, 4, 1, 6}
	cpsPhPower = []uint32{4, 4, 1, 7}
	cpsPhL2L   = []uint32{4, 4, 1, 13}
	cpsBank    = []uint32{5, 4, 1, 5}
	cpsOName   = []uint32{6, 1, 4, 1, 4} // switched: 1 on, 2 off
	cpsOState  = []uint32{6, 1, 4, 1, 5}
	cpsOBank   = []uint32{6, 1, 3, 1, 6}
	cpsMName   = []uint32{6, 2, 4, 1, 4}
	cpsMLoad   = []uint32{6, 2, 4, 1, 6}
	cpsMPower  = []uint32{6, 2, 4, 1, 7}
	cpsMBank   = []uint32{6, 2, 3, 1, 7}
	cpsMonName = []uint32{6, 3, 4, 1, 4}
	cpsMonBank = []uint32{6, 3, 3, 1, 7}
	cpsCommand = []uint32{6, 1, 5, 1, 5} // 1 on, 2 off (read: the same)

	// legacy ePDU
	cplName    = []uint32{1, 1}
	cplFirm    = []uint32{1, 3}
	cplModel   = []uint32{1, 5}
	cplSerial  = []uint32{1, 6}
	cplOutlets = []uint32{1, 8}
	cplPhases  = []uint32{1, 9}
	cplBreaker = []uint32{1, 10}
	cplVoltage = []uint32{1, 15}
	cplLoad    = []uint32{2, 3, 1, 1, 2}
	cplLPhase  = []uint32{2, 3, 1, 1, 4}
	cplLBank   = []uint32{2, 3, 1, 1, 5}
	cplLVolt   = []uint32{2, 3, 1, 1, 6}
	cplLPower  = []uint32{2, 3, 1, 1, 7}
	cplCName   = []uint32{3, 3, 1, 1, 2}
	cplCCmd    = []uint32{3, 3, 1, 1, 4}
	cplCBank   = []uint32{3, 3, 1, 1, 5}
	cplSName   = []uint32{3, 5, 1, 1, 2}
	cplSState  = []uint32{3, 5, 1, 1, 4}
	cplSBank   = []uint32{3, 5, 1, 1, 6}
	cplSLoad   = []uint32{3, 5, 1, 1, 7}
	cplSPower  = []uint32{3, 5, 1, 1, 8}
)

const (
	flavorUnknown = iota
	flavorNew     // ePDU2 / rPDU2
	flavorLegacy
)

func (d *cyberPower) detect(ctx context.Context) (int, error) {
	d.mu.Lock()
	f := d.flavor
	d.mu.Unlock()
	if f != flavorUnknown {
		return f, nil
	}
	probe, err := snmp.GetAvailable(ctx, d.t, []snmp.OID{oid(cpsRoot, cpsOutlets, 1), cpsLegacy.Append(append(append([]uint32{}, cplOutlets...), 0)...)}, 8)
	if err != nil {
		return 0, err
	}
	found := flavorUnknown
	if n, _ := probe.Int(oid(cpsRoot, cpsOutlets, 1)); n > 0 {
		found = flavorNew
	} else if n, _ := probe.Int(cpsLegacy.Append(append(append([]uint32{}, cplOutlets...), 0)...)); n > 0 {
		found = flavorLegacy
	} else {
		return 0, &ErrNotRecognized{Vendor: "CyberPower"}
	}
	d.mu.Lock()
	d.flavor = found
	d.mu.Unlock()
	return found, nil
}

func (d *cyberPower) Poll(ctx context.Context) (*Snapshot, error) {
	f, err := d.detect(ctx)
	if err != nil {
		return nil, err
	}
	if f == flavorNew {
		return d.pollNew(ctx)
	}
	return d.pollLegacy(ctx)
}

func (d *cyberPower) SetOutlet(ctx context.Context, n int, on bool) error {
	f, err := d.detect(ctx)
	if err != nil {
		return err
	}
	target := oid(cpsRoot, cpsCommand, n)
	if f == flavorLegacy {
		target = oid(cpsLegacy, cplCCmd, n)
	}
	v := int64(2)
	if on {
		v = 1
	}
	return d.t.Set(ctx, []snmp.VarBind{{OID: target, Value: snmp.Integer(v)}})
}

func (d *cyberPower) OutletState(ctx context.Context, n int) (*bool, error) {
	f, err := d.detect(ctx)
	if err != nil {
		return nil, err
	}
	target := oid(cpsRoot, cpsOState, n)
	if f == flavorLegacy {
		target = oid(cpsLegacy, cplSState, n)
	}
	v, err := snmp.GetAvailable(ctx, d.t, []snmp.OID{target}, 1)
	if err != nil {
		return nil, err
	}
	state, ok := v.Int(target)
	if !ok {
		return nil, nil
	}
	return ptr(state == 1), nil
}

func (d *cyberPower) pollNew(ctx context.Context) (*Snapshot, error) {
	head, err := snmp.GetAvailable(ctx, d.t, []snmp.OID{
		oid(cpsRoot, cpsModel, 1), oid(cpsRoot, cpsName, 1), oid(cpsRoot, cpsSerial, 1), oid(cpsRoot, cpsFirm, 1),
		oid(cpsRoot, cpsOutlets, 1), oid(cpsRoot, cpsPhases, 1), oid(cpsRoot, cpsBreaker, 1), oid(cpsRoot, cpsOrient, 1),
		oid(cpsRoot, cpsDevAmps, 1), oid(cpsRoot, cpsDevWatt, 1),
	}, 8)
	if err != nil {
		return nil, err
	}
	outletCount, _ := head.Int(oid(cpsRoot, cpsOutlets, 1))
	if outletCount <= 0 {
		return nil, &ErrNotRecognized{Vendor: "CyberPower"}
	}
	phaseCount, ok := head.Int(oid(cpsRoot, cpsPhases, 1))
	if !ok || phaseCount < 1 {
		phaseCount = 1
	}
	if phaseCount > 3 {
		phaseCount = 3
	}
	declared, haveDeclared := head.Int(oid(cpsRoot, cpsBreaker, 1))
	bankCount := declared
	if bankCount > 8 {
		bankCount = 8
	}
	d.mu.Lock()
	known := d.tables
	d.mu.Unlock()

	var want []snmp.OID
	for p := 1; p <= phaseCount; p++ {
		want = append(want, oid(cpsRoot, cpsPhLoad, p), oid(cpsRoot, cpsPhVolt, p), oid(cpsRoot, cpsPhPower, p), oid(cpsRoot, cpsPhL2L, p))
	}
	for b := 1; b <= bankCount; b++ {
		want = append(want, oid(cpsRoot, cpsBank, b))
	}
	for o := 1; o <= outletCount; o++ {
		if !known.known || known.switched {
			want = append(want, oid(cpsRoot, cpsOName, o), oid(cpsRoot, cpsOState, o), oid(cpsRoot, cpsOBank, o))
		}
		if !known.known || known.metered {
			want = append(want, oid(cpsRoot, cpsMName, o), oid(cpsRoot, cpsMLoad, o), oid(cpsRoot, cpsMPower, o), oid(cpsRoot, cpsMBank, o))
		}
		if !known.known || known.monit {
			want = append(want, oid(cpsRoot, cpsMonName, o), oid(cpsRoot, cpsMonBank, o))
		}
	}
	values, err := snmp.GetAvailable(ctx, d.t, want, 8)
	if err != nil {
		return nil, err
	}
	if !known.known {
		found := tableSet{known: true,
			switched: values.Has(oid(cpsRoot, cpsOState, 1)),
			metered:  values.Has(oid(cpsRoot, cpsMLoad, 1)) || values.Has(oid(cpsRoot, cpsMPower, 1)),
			monit:    values.Has(oid(cpsRoot, cpsMonName, 1))}
		d.mu.Lock()
		d.tables = found
		d.mu.Unlock()
	}

	snap := &Snapshot{}
	for p := 1; p <= phaseCount; p++ {
		amps, ok := values.Tenths(oid(cpsRoot, cpsPhLoad, p))
		if !ok {
			continue
		}
		volts, vok := values.Tenths(oid(cpsRoot, cpsPhVolt, p))
		snap.Phases = append(snap.Phases, Phase{Number: p, Amps: amps, Watts: wattsOf(values, oid(cpsRoot, cpsPhPower, p)), Volts: asFloatPtr(volts, vok)})
	}
	for o := 1; o <= outletCount; o++ {
		name, _ := values.String(oid(cpsRoot, cpsOName, o))
		if name == "" {
			name, _ = values.String(oid(cpsRoot, cpsMName, o))
		}
		if name == "" {
			name, _ = values.String(oid(cpsRoot, cpsMonName, o))
		}
		bank, bok := values.Int(oid(cpsRoot, cpsOBank, o))
		if !bok {
			bank, bok = values.Int(oid(cpsRoot, cpsMBank, o))
		}
		if !bok {
			bank, bok = values.Int(oid(cpsRoot, cpsMonBank, o))
		}
		out := Outlet{Number: o, Name: cleanName(name), Bank: intPtr(bank, bok)}
		if s, ok := values.Int(oid(cpsRoot, cpsOState, o)); ok {
			out.On = ptr(s == 1)
		}
		amps, aok := values.Tenths(oid(cpsRoot, cpsMLoad, o))
		out.Amps = asFloatPtr(amps, aok)
		out.Watts = wattsOf(values, oid(cpsRoot, cpsMPower, o))
		snap.Outlet = append(snap.Outlet, out)
	}
	// Banks: the declared ones, or when the device does not declare any, the ones it answers for.
	bankWant := bankCount
	if !haveDeclared {
		bankWant = 4
		var probe []snmp.OID
		for b := 1; b <= bankWant; b++ {
			probe = append(probe, oid(cpsRoot, cpsBank, b))
		}
		extra, err := snmp.GetAvailable(ctx, d.t, probe, 8)
		if err != nil {
			return nil, err
		}
		for k, v := range extra {
			values[k] = v
		}
	}
	for b := 1; b <= bankWant; b++ {
		if amps, ok := values.Tenths(oid(cpsRoot, cpsBank, b)); ok {
			snap.Banks = append(snap.Banks, Bank{Number: b, Amps: amps})
		}
	}
	info := Info{Vendor: CyberPower, Outlets: outletCount}
	info.Model, _ = head.String(oid(cpsRoot, cpsModel, 1))
	info.Name, _ = head.String(oid(cpsRoot, cpsName, 1))
	info.Serial, _ = head.String(oid(cpsRoot, cpsSerial, 1))
	info.Firmware, _ = head.String(oid(cpsRoot, cpsFirm, 1))
	if len(snap.Banks) > 0 {
		info.Breakers = ptr(len(snap.Banks))
	}
	if o, ok := head.Int(oid(cpsRoot, cpsOrient, 1)); ok {
		info.Orientation = map[bool]string{true: "Vertical", false: "Horizontal"}[o == 2]
	}
	if v, ok := values.Tenths(oid(cpsRoot, cpsPhL2L, 1)); ok {
		info.LineVoltage = ptr(v)
	} else if len(snap.Phases) > 0 {
		info.LineVoltage = snap.Phases[0].Volts
	}
	snap.Info = info
	if len(snap.Phases) <= 1 {
		if a, ok := head.Tenths(oid(cpsRoot, cpsDevAmps, 1)); ok {
			snap.TotalAmps = ptr(a)
		}
	}
	snap.TotalWatts = wattsOf(head, oid(cpsRoot, cpsDevWatt, 1))
	snap.finish()
	return snap, nil
}

func legacyScalar(path []uint32) snmp.OID {
	return cpsLegacy.Append(append(append([]uint32{}, path...), 0)...)
}

func (d *cyberPower) pollLegacy(ctx context.Context) (*Snapshot, error) {
	head, err := snmp.GetAvailable(ctx, d.t, []snmp.OID{
		legacyScalar(cplName), legacyScalar(cplFirm), legacyScalar(cplModel), legacyScalar(cplSerial),
		legacyScalar(cplOutlets), legacyScalar(cplPhases), legacyScalar(cplBreaker), legacyScalar(cplVoltage),
	}, 8)
	if err != nil {
		return nil, err
	}
	outletCount, _ := head.Int(legacyScalar(cplOutlets))
	if outletCount <= 0 {
		return nil, &ErrNotRecognized{Vendor: "CyberPower"}
	}
	var want []snmp.OID
	for i := 1; i <= 10; i++ { // the load table lists the phases first and then the banks
		want = append(want, oid(cpsLegacy, cplLoad, i), oid(cpsLegacy, cplLPhase, i), oid(cpsLegacy, cplLBank, i), oid(cpsLegacy, cplLVolt, i), oid(cpsLegacy, cplLPower, i))
	}
	for o := 1; o <= outletCount; o++ {
		want = append(want, oid(cpsLegacy, cplCName, o), oid(cpsLegacy, cplCBank, o), oid(cpsLegacy, cplSName, o), oid(cpsLegacy, cplSState, o),
			oid(cpsLegacy, cplSBank, o), oid(cpsLegacy, cplSLoad, o), oid(cpsLegacy, cplSPower, o))
	}
	values, err := snmp.GetAvailable(ctx, d.t, want, 8)
	if err != nil {
		return nil, err
	}
	snap := &Snapshot{}
	for i := 1; i <= 10; i++ {
		amps, ok := values.Tenths(oid(cpsLegacy, cplLoad, i))
		if !ok {
			continue
		}
		bank, _ := values.Int(oid(cpsLegacy, cplLBank, i))
		watts := wattsOf(values, oid(cpsLegacy, cplLPower, i))
		if bank > 0 {
			snap.Banks = append(snap.Banks, Bank{Number: bank, Amps: amps, Watts: watts})
			continue
		}
		number, nok := values.Int(oid(cpsLegacy, cplLPhase, i))
		if !nok {
			number = len(snap.Phases) + 1
		}
		volts, vok := values.Tenths(oid(cpsLegacy, cplLVolt, i))
		snap.Phases = append(snap.Phases, Phase{Number: number, Amps: amps, Watts: watts, Volts: asFloatPtr(volts, vok)})
	}
	for o := 1; o <= outletCount; o++ {
		name, _ := values.String(oid(cpsLegacy, cplCName, o))
		if name == "" {
			name, _ = values.String(oid(cpsLegacy, cplSName, o))
		}
		bank, bok := values.Int(oid(cpsLegacy, cplCBank, o))
		if !bok {
			bank, bok = values.Int(oid(cpsLegacy, cplSBank, o))
		}
		out := Outlet{Number: o, Name: cleanName(name), Bank: intPtr(bank, bok)}
		if s, ok := values.Int(oid(cpsLegacy, cplSState, o)); ok && (s == 1 || s == 2) {
			out.On = ptr(s == 1)
		}
		amps, aok := values.Tenths(oid(cpsLegacy, cplSLoad, o))
		out.Amps = asFloatPtr(amps, aok)
		out.Watts = wattsOf(values, oid(cpsLegacy, cplSPower, o))
		snap.Outlet = append(snap.Outlet, out)
	}
	sortBanks(snap.Banks)
	info := Info{Vendor: CyberPower, Outlets: outletCount}
	info.Model, _ = head.String(legacyScalar(cplModel))
	info.Name, _ = head.String(legacyScalar(cplName))
	info.Serial, _ = head.String(legacyScalar(cplSerial))
	info.Firmware, _ = head.String(legacyScalar(cplFirm))
	if len(snap.Banks) > 0 {
		info.Breakers = ptr(len(snap.Banks))
	} else if b, ok := head.Int(legacyScalar(cplBreaker)); ok {
		info.Breakers = ptr(b)
	}
	if v, ok := head.Int(legacyScalar(cplVoltage)); ok && v > 0 {
		info.LineVoltage = ptr(float64(v))
	} else if len(snap.Phases) > 0 {
		info.LineVoltage = snap.Phases[0].Volts
	}
	snap.Info = info
	snap.finish()
	return snap, nil
}

func sortBanks(b []Bank) {
	for i := 1; i < len(b); i++ {
		for j := i; j > 0 && b[j].Number < b[j-1].Number; j-- {
			b[j], b[j-1] = b[j-1], b[j]
		}
	}
}
