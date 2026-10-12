package pdu_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"testing"

	"github.com/kblaskoff/pdu-monitor/server/internal/pdu"
	"github.com/kblaskoff/pdu-monitor/server/internal/snmp"
	"github.com/kblaskoff/pdu-monitor/server/internal/snmp/snmptest"
)

// The OIDs in these tests are typed out from the vendor MIB files (CPS-MIB and PowerNet-MIB), on purpose not shared with the drivers.
const (
	cps = "1.3.6.1.4.1.3808.1.1.6"
	apc = "1.3.6.1.4.1.318.1.1"
)

func approx(t *testing.T, name string, got *float64, want float64) {
	t.Helper()
	if got == nil || math.Abs(*got-want) > 0.001 {
		var g any = "nil"
		if got != nil {
			g = *got
		}
		t.Errorf("%s: got %v, want %v", name, g, want)
	}
}

func newDriver(t *testing.T, v pdu.Vendor, a *snmptest.Agent) pdu.Driver {
	t.Helper()
	d, err := pdu.NewDriver(v, a.Client())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// P200B of the sample screen: 16 outlets, 2 banks, 230 V.
func cyberPowerAgent(t *testing.T) *snmptest.Agent {
	a := snmptest.New(t)
	a.SetString(cps+".2.2.1.9.1", "PDU81007")
	a.SetString(cps+".2.2.1.3.1", "P200B")
	a.SetInt(cps+".3.3.1.5.1", 16)
	a.SetInt(cps+".3.3.1.8.1", 1)
	a.SetInt(cps+".3.3.1.9.1", 2)
	a.SetInt(cps+".3.3.1.11.1", 1)
	a.SetUint(cps+".3.4.1.5.1", 112)
	a.SetUint(cps+".3.4.1.18.1", 2150)
	a.SetUint(cps+".4.4.1.5.1", 112)
	a.SetUint(cps+".4.4.1.7.1", 2150)
	a.SetUint(cps+".4.4.1.13.1", 2300)
	a.SetUint(cps+".5.4.1.5.1", 65)
	a.SetUint(cps+".5.4.1.5.2", 47)
	names := []string{"200U31", "200U24", "200U16", "200U29", "200U27", "200U18", "Outlet_7", "200U3",
		"200U2", "200U34", "200U22", "200U12-1", "Outlet_13", "200U13", "Outlet_15", "200U20"}
	tenths := []uint64{2, 5, 18, 5, 5, 20, 0, 10, 0, 4, 5, 4, 0, 15, 0, 19}
	watts := []uint64{10, 80, 360, 100, 100, 390, 0, 200, 0, 80, 90, 70, 0, 300, 0, 370}
	for i := 1; i <= 16; i++ {
		a.SetString(fmt.Sprintf("%s.6.1.4.1.4.%d", cps, i), names[i-1])
		a.SetInt(fmt.Sprintf("%s.6.1.4.1.5.%d", cps, i), 1)
		a.SetInt(fmt.Sprintf("%s.6.1.5.1.5.%d", cps, i), 1)
		bank := int64(1)
		if i > 8 {
			bank = 2
		}
		a.SetInt(fmt.Sprintf("%s.6.1.3.1.6.%d", cps, i), bank)
		a.SetUint(fmt.Sprintf("%s.6.2.4.1.6.%d", cps, i), tenths[i-1])
		a.SetUint(fmt.Sprintf("%s.6.2.4.1.7.%d", cps, i), watts[i-1])
	}
	a.OnSet(func(a *snmptest.Agent, o snmp.OID, v snmp.Value) { // control .6.1.5.1.5.<i> mirrors into status .6.1.4.1.5.<i>
		if len(o) > 6 && o[len(o)-6] == 6 && o[len(o)-5] == 1 && o[len(o)-4] == 5 && o[len(o)-3] == 1 && o[len(o)-2] == 5 {
			a.SetInt(fmt.Sprintf("%s.6.1.4.1.5.%d", cps, o[len(o)-1]), v.Int)
		}
	})
	return a
}

func TestCyberPowerSnapshotMatchesTheSampleScreen(t *testing.T) {
	a := cyberPowerAgent(t)
	snap, err := newDriver(t, pdu.CyberPower, a).Poll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snap.Info.Model != "PDU81007" || snap.Info.Name != "P200B" || snap.Info.Outlets != 16 || *snap.Info.Breakers != 2 || snap.Info.Orientation != "Horizontal" {
		t.Errorf("info %+v", snap.Info)
	}
	approx(t, "voltage", snap.Info.LineVoltage, 230)
	if len(snap.Outlet) != 16 {
		t.Fatal(len(snap.Outlet))
	}
	o := snap.Outlet[2]
	if o.Name != "200U16" || *o.On != true || *o.Bank != 1 || *snap.Outlet[15].Bank != 2 {
		t.Errorf("outlet 3: %+v", o)
	}
	approx(t, "outlet amps", o.Amps, 1.8)
	approx(t, "outlet watts", o.Watts, 360)
	if snap.Outlet[6].Assigned() {
		t.Error("Outlet_7 is not a server")
	}
	approx(t, "total amps", snap.TotalAmps, 11.2)
	approx(t, "total watts", snap.TotalWatts, 2150)
	if len(snap.Banks) != 2 || snap.Banks[0].Amps != 6.5 || snap.Banks[1].Amps != 4.7 {
		t.Errorf("banks %+v", snap.Banks)
	}
	approx(t, "bank 1 watts", snap.Banks[0].Watts, 1240) // from the outlets: 10+80+360+100+100+390+0+200
	approx(t, "bank 2 watts", snap.Banks[1].Watts, 910)
	if !snap.HasOutletMetering() {
		t.Error("outlets are metered")
	}
}

func TestCyberPowerSwitchingUsesTheWriteCommunityAndTheRightValues(t *testing.T) {
	a := cyberPowerAgent(t)
	d := newDriver(t, pdu.CyberPower, a)
	ctx := context.Background()
	if err := d.SetOutlet(ctx, 3, false); err != nil {
		t.Fatal(err)
	}
	if v, _ := a.Get(cps + ".6.1.5.1.5.3"); v.Int != 2 {
		t.Fatalf("command %d", v.Int)
	}
	if s, err := d.OutletState(ctx, 3); err != nil || s == nil || *s {
		t.Fatalf("state %v %v", s, err)
	}
	if err := d.SetOutlet(ctx, 3, true); err != nil {
		t.Fatal(err)
	}
	if s, _ := d.OutletState(ctx, 3); s == nil || !*s {
		t.Fatal("not on again")
	}
	for _, r := range a.Requests() {
		if r.Type == snmp.SetRequest && r.Community != "private" {
			t.Errorf("SET with community %q", r.Community)
		}
	}
}

func TestReadOnlyCommunityCannotSwitch(t *testing.T) {
	a := cyberPowerAgent(t)
	c := a.Client()
	c.WriteCommunity = ""
	d, _ := pdu.NewDriver(pdu.CyberPower, c)
	if err := d.SetOutlet(context.Background(), 1, false); !errors.Is(err, snmp.ErrTimeout) { // the agent ignores a SET with the wrong community
		t.Fatalf("got %v", err)
	}
}

func TestAnUnknownDeviceIsReportedAsNotRecognized(t *testing.T) {
	a := snmptest.New(t)
	a.SetString("1.3.6.1.2.1.1.1.0", "something else")
	for _, v := range []pdu.Vendor{pdu.CyberPower, pdu.APC} {
		_, err := newDriver(t, v, a).Poll(context.Background())
		var nr *pdu.ErrNotRecognized
		if !errors.As(err, &nr) {
			t.Errorf("%s: %v", v, err)
		}
	}
}

// AP7932-like: 24 outlets, 2 banks, switched but only metered per bank (no outlet metering), rPDU2 firmware.
func apcAgent(t *testing.T) *snmptest.Agent {
	a := snmptest.New(t)
	a.SetString(apc+".26.2.1.3.1", "P200A")
	a.SetString(apc+".26.2.1.8.1", "AP7932")
	a.SetInt(apc+".26.4.2.1.4.1", 24)
	a.SetInt(apc+".26.4.2.1.7.1", 1)
	a.SetInt(apc+".26.4.2.1.8.1", 2)
	a.SetInt(apc+".26.4.2.1.10.1", 2)
	a.SetInt(apc+".26.4.3.1.5.1", -1)
	a.SetInt(apc+".26.6.3.1.5.1", 128)
	a.SetInt(apc+".26.6.3.1.6.1", 230)
	a.SetInt(apc+".26.6.3.1.7.1", 233) // hundredths of kW = 2330 W
	a.SetInt(apc+".26.8.3.1.5.1", 55)
	a.SetInt(apc+".26.8.3.1.5.2", 73)
	for i := 1; i <= 24; i++ {
		name := fmt.Sprintf("Outlet_%d", i)
		if i == 1 {
			name = "200U31"
		}
		a.SetString(fmt.Sprintf("%s.26.9.2.3.1.3.%d", apc, i), name)
		a.SetInt(fmt.Sprintf("%s.26.9.2.3.1.5.%d", apc, i), 2) // on (in this MIB 1 is off!)
		bank := int64(1)
		if i > 12 {
			bank = 2
		}
		a.SetInt(fmt.Sprintf("%s.26.9.2.2.1.6.%d", apc, i), bank)
		a.SetInt(fmt.Sprintf("%s.26.9.2.4.1.5.%d", apc, i), 1)
	}
	a.OnSet(func(a *snmptest.Agent, o snmp.OID, v snmp.Value) { // .9.2.4.1.5.<i> mirrors into .9.2.3.1.5.<i>
		if len(o) > 6 && o[len(o)-6] == 9 && o[len(o)-5] == 2 && o[len(o)-4] == 4 && o[len(o)-3] == 1 && o[len(o)-2] == 5 {
			state := int64(1)
			if v.Int == 1 {
				state = 2
			}
			a.SetInt(fmt.Sprintf("%s.26.9.2.3.1.5.%d", apc, o[len(o)-1]), state)
		}
	})
	return a
}

func TestAPCRPDU2SnapshotAndSwitching(t *testing.T) {
	a := apcAgent(t)
	d := newDriver(t, pdu.APC, a)
	ctx := context.Background()
	snap, err := d.Poll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Info.Model != "AP7932" || len(snap.Outlet) != 24 || snap.Outlet[0].Name != "200U31" || !*snap.Outlet[0].On {
		t.Errorf("%+v", snap.Info)
	}
	if snap.Outlet[0].Amps != nil || snap.Outlet[0].Watts != nil || snap.HasOutletMetering() {
		t.Error("unmetered outlets must be nil, not 0")
	}
	approx(t, "amps", snap.TotalAmps, 12.8)
	approx(t, "watts", snap.TotalWatts, 2330) // from the phase: the device-level watts said -1
	if len(snap.Banks) != 2 || snap.Banks[0].Amps != 5.5 || snap.Banks[1].Amps != 7.3 || *snap.Outlet[23].Bank != 2 {
		t.Errorf("banks %+v", snap.Banks)
	}
	if err := d.SetOutlet(ctx, 1, false); err != nil {
		t.Fatal(err)
	}
	if s, _ := d.OutletState(ctx, 1); s == nil || *s {
		t.Fatal("should be off")
	}
	if err := d.SetOutlet(ctx, 1, true); err != nil {
		t.Fatal(err)
	}
	if s, _ := d.OutletState(ctx, 1); s == nil || !*s {
		t.Fatal("should be on")
	}
}

func TestAPCLegacyFirmware(t *testing.T) {
	a := snmptest.New(t)
	a.SetString(apc+".12.1.5.0", "AP7932")
	a.SetInt(apc+".12.1.8.0", 24)
	a.SetInt(apc+".12.1.9.0", 1)
	a.SetUint(apc+".12.2.3.1.1.2.1", 128)
	a.SetInt(apc+".12.2.3.1.1.4.1", 1)
	a.SetInt(apc+".12.2.3.1.1.5.1", 0)
	a.SetUint(apc+".12.2.3.1.1.2.2", 55)
	a.SetInt(apc+".12.2.3.1.1.4.2", 1)
	a.SetInt(apc+".12.2.3.1.1.5.2", 1)
	a.SetUint(apc+".12.2.3.1.1.2.3", 73)
	a.SetInt(apc+".12.2.3.1.1.4.3", 1)
	a.SetInt(apc+".12.2.3.1.1.5.3", 2)
	for i := 1; i <= 24; i++ {
		name := fmt.Sprintf("Outlet %d", i)
		if i == 2 {
			name = "200U29"
		}
		a.SetString(fmt.Sprintf("%s.4.4.2.1.4.%d", apc, i), name)
		a.SetInt(fmt.Sprintf("%s.4.4.2.1.3.%d", apc, i), 1)
	}
	d := newDriver(t, pdu.APC, a)
	snap, err := d.Poll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Outlet) != 24 || snap.Outlet[1].Name != "200U29" || len(snap.Phases) != 1 || len(snap.Banks) != 2 {
		t.Fatalf("%+v", snap)
	}
	approx(t, "amps", snap.TotalAmps, 12.8)
	if err := d.SetOutlet(context.Background(), 2, false); err != nil {
		t.Fatal(err)
	}
	if v, _ := a.Get(apc + ".4.4.2.1.3.2"); v.Int != 2 {
		t.Fatalf("command %d", v.Int)
	}
	if s, _ := d.OutletState(context.Background(), 2); s == nil || *s {
		t.Fatal("should be off")
	}
}

func TestDeviceWithASmallPacketLimitStillWorks(t *testing.T) {
	// error-status 1 (tooBig) was what a real PDU answered to a request with 16 objects
	a := cyberPowerAgent(t)
	a.LimitVarBinds(3)
	snap, err := newDriver(t, pdu.CyberPower, a).Poll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Outlet) != 16 {
		t.Fatal(len(snap.Outlet))
	}
	approx(t, "watts", snap.Outlet[2].Watts, 360)
	approx(t, "total", snap.TotalWatts, 2150)
	b := apcAgent(t)
	b.LimitVarBinds(1)
	apcSnap, err := newDriver(t, pdu.APC, b).Poll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(apcSnap.Outlet) != 24 || apcSnap.Banks[1].Amps != 7.3 {
		t.Fatalf("%+v", apcSnap.Banks)
	}
}

func TestCyberPowerLegacyEPDU(t *testing.T) {
	a := snmptest.New(t)
	l := "1.3.6.1.4.1.3808.1.1.3"
	a.SetString(l+".1.5.0", "PDU20SWHVIEC8FNET")
	a.SetInt(l+".1.8.0", 8)
	a.SetInt(l+".1.9.0", 1)
	a.SetUint(l+".2.3.1.1.2.1", 52)
	a.SetInt(l+".2.3.1.1.4.1", 1)
	a.SetInt(l+".2.3.1.1.5.1", 0)
	a.SetInt(l+".2.3.1.1.7.1", 1196)
	for i := 1; i <= 8; i++ {
		name := fmt.Sprintf("Outlet%d", i)
		if i == 1 {
			name = "100U5"
		}
		a.SetString(fmt.Sprintf("%s.3.3.1.1.2.%d", l, i), name)
		a.SetInt(fmt.Sprintf("%s.3.3.1.1.4.%d", l, i), 1)
		a.SetInt(fmt.Sprintf("%s.3.5.1.1.4.%d", l, i), 1)
		if i == 1 {
			a.SetUint(fmt.Sprintf("%s.3.5.1.1.7.%d", l, i), 12)
			a.SetUint(fmt.Sprintf("%s.3.5.1.1.8.%d", l, i), 276)
		}
	}
	a.OnSet(func(a *snmptest.Agent, o snmp.OID, v snmp.Value) {
		if len(o) > 6 && o[len(o)-6] == 3 && o[len(o)-5] == 3 && o[len(o)-4] == 1 && o[len(o)-3] == 1 && o[len(o)-2] == 4 {
			a.SetInt(fmt.Sprintf("%s.3.5.1.1.4.%d", l, o[len(o)-1]), v.Int)
		}
	})
	d := newDriver(t, pdu.CyberPower, a)
	snap, err := d.Poll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snap.Info.Model != "PDU20SWHVIEC8FNET" || len(snap.Outlet) != 8 || snap.Outlet[0].Name != "100U5" {
		t.Fatalf("%+v", snap.Info)
	}
	approx(t, "watts", snap.Outlet[0].Watts, 276)
	approx(t, "amps", snap.TotalAmps, 5.2)
	approx(t, "total watts", snap.TotalWatts, 1196)
	if err := d.SetOutlet(context.Background(), 1, false); err != nil {
		t.Fatal(err)
	}
	if v, _ := a.Get(l + ".3.3.1.1.4.1"); v.Int != 2 {
		t.Fatalf("command %d", v.Int)
	}
	if s, _ := d.OutletState(context.Background(), 1); s == nil || *s {
		t.Fatal("should be off")
	}
}

func TestCyberPowerMonitoredOnlyModelShowsNamesWithoutSwitching(t *testing.T) {
	a := snmptest.New(t)
	a.SetInt(cps+".3.3.1.5.1", 4)
	a.SetInt(cps+".3.3.1.8.1", 1)
	a.SetUint(cps+".3.4.1.5.1", 60)
	a.SetUint(cps+".4.4.1.5.1", 60)
	for i := 1; i <= 4; i++ {
		name := fmt.Sprintf("Outlet_%d", i)
		if i == 2 {
			name = "300U7"
		}
		a.SetString(fmt.Sprintf("%s.6.3.4.1.4.%d", cps, i), name)
	}
	snap, err := newDriver(t, pdu.CyberPower, a).Poll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snap.Outlet[1].Name != "300U7" || snap.Outlet[1].On != nil || snap.Outlet[1].Amps != nil || len(snap.Banks) != 0 {
		t.Fatalf("%+v", snap.Outlet[1])
	}
	approx(t, "total", snap.TotalAmps, 6.0)
}

func TestThreePhasePDUUsesTheBusiestPhase(t *testing.T) {
	a := snmptest.New(t)
	a.SetInt(cps+".3.3.1.5.1", 2)
	a.SetInt(cps+".3.3.1.8.1", 3)
	for i, tenths := range []uint64{100, 160, 80} {
		a.SetUint(fmt.Sprintf("%s.4.4.1.5.%d", cps, i+1), tenths)
		a.SetUint(fmt.Sprintf("%s.4.4.1.7.%d", cps, i+1), 2000)
	}
	snap, err := newDriver(t, pdu.CyberPower, a).Poll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Phases) != 3 {
		t.Fatal(len(snap.Phases))
	}
	approx(t, "amps", snap.TotalAmps, 16.0)
	approx(t, "watts", snap.TotalWatts, 6000)
}

func TestAPCMeteredOnlyRPDU2(t *testing.T) {
	a := snmptest.New(t)
	a.SetInt(apc+".26.4.2.1.4.1", 8)
	a.SetInt(apc+".26.4.2.1.7.1", 1)
	a.SetInt(apc+".26.6.3.1.5.1", 24)
	for i := 1; i <= 8; i++ {
		name := fmt.Sprintf("Outlet_%d", i)
		cur, pow := int64(0), int64(0)
		if i == 3 {
			name, cur, pow = "N100U40", 12, 276
		}
		a.SetString(fmt.Sprintf("%s.26.9.4.3.1.3.%d", apc, i), name)
		a.SetInt(fmt.Sprintf("%s.26.9.4.3.1.6.%d", apc, i), cur)
		a.SetInt(fmt.Sprintf("%s.26.9.4.3.1.7.%d", apc, i), pow)
	}
	d := newDriver(t, pdu.APC, a)
	for round := 0; round < 2; round++ { // the second poll asks only for the tables the first one found
		snap, err := d.Poll(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		o := snap.Outlet[2]
		if o.Name != "N100U40" || o.On != nil {
			t.Fatalf("round %d: %+v", round, o)
		}
		approx(t, "watts", o.Watts, 276)
		approx(t, "amps", o.Amps, 1.2)
	}
}
