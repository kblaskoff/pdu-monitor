package pdu_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/kblaskoff/pdu-monitor/server/internal/pdu"
	"github.com/kblaskoff/pdu-monitor/server/internal/snmp"
	"github.com/kblaskoff/pdu-monitor/server/internal/snmp/snmptest"
)

func namer(t *testing.T, v pdu.Vendor, a *snmptest.Agent) pdu.Namer {
	t.Helper()
	n, ok := newDriver(t, v, a).(pdu.Namer)
	if !ok {
		t.Fatal("the driver cannot name outlets")
	}
	return n
}

func sets(a *snmptest.Agent) []*snmp.Message {
	var out []*snmp.Message
	for _, r := range a.Requests() {
		if r.Type == snmp.SetRequest {
			out = append(out, r)
		}
	}
	return out
}

// seed makes the writable name objects exist, like on a real device (a SET to an object that is not there is refused)
func seed(a *snmptest.Agent, prefix string, count int) {
	for i := 1; i <= count; i++ {
		a.SetString(fmt.Sprintf("%s.%d", prefix, i), "")
	}
}

// the name is written to the Config table (read-write) and the device shows it in the Status table (read-only) that the driver reads
func mirror(a *snmptest.Agent, from, to string) {
	a.OnSet(func(a *snmptest.Agent, o snmp.OID, v snmp.Value) {
		s := o.String()
		if i := strings.Index(s, from); i >= 0 {
			a.Set(s[:i]+to+s[i+len(from):], v)
		}
	})
}

func TestCyberPowerNameGoesToTheConfigTableAndShowsInTheStatus(t *testing.T) {
	a := cyberPowerAgent(t)
	seed(a, cps+".6.1.2.1.4", 16)
	mirror(a, ".6.1.2.1.4.", ".6.1.4.1.4.")
	d := namer(t, pdu.CyberPower, a)
	if err := d.SetOutletName(context.Background(), 7, "200U13"); err != nil {
		t.Fatal(err)
	}
	got := sets(a)
	if len(got) != 1 || got[0].Community != "private" || len(got[0].VarBinds) != 1 {
		t.Fatalf("%+v", got)
	}
	vb := got[0].VarBinds[0]
	if vb.OID.String() != cps+".6.1.2.1.4.7" || vb.Value.Kind != snmp.KindOctetString || string(vb.Value.Bytes) != "200U13" {
		t.Fatalf("%s %v", vb.OID, vb.Value)
	}
	snap, err := newDriver(t, pdu.CyberPower, a).Poll(context.Background())
	if err != nil || snap.Outlet[6].Name != "200U13" || snap.Outlet[5].Name != "200U18" {
		t.Fatalf("%v %+v", err, snap.Outlet[5:7])
	}
}

func TestCyberPowerMeteredAndMonitoredModelsUseTheirOwnConfigTables(t *testing.T) {
	for _, c := range []struct{ table, name string }{{"6.2", "metered"}, {"6.3", "monitored"}} {
		a := snmptest.New(t)
		a.SetInt(cps+".3.3.1.5.1", 4)
		a.SetInt(cps+".3.3.1.8.1", 1)
		a.SetUint(cps+".3.4.1.5.1", 60)
		a.SetUint(cps+".4.4.1.5.1", 60)
		for i := 1; i <= 4; i++ {
			a.SetString(fmt.Sprintf("%s.%s.4.1.4.%d", cps, c.table, i), fmt.Sprintf("Outlet_%d", i))
			if c.table == "6.2" {
				a.SetUint(fmt.Sprintf("%s.6.2.4.1.6.%d", cps, i), 0)
				a.SetUint(fmt.Sprintf("%s.6.2.4.1.7.%d", cps, i), 0)
			}
		}
		seed(a, fmt.Sprintf("%s.%s.2.1.4", cps, c.table), 4)
		if err := namer(t, pdu.CyberPower, a).SetOutletName(context.Background(), 2, "300U7"); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		got := sets(a)
		if len(got) != 1 || got[0].VarBinds[0].OID.String() != fmt.Sprintf("%s.%s.2.1.4.2", cps, c.table) {
			t.Errorf("%s: %+v", c.name, got)
		}
	}
}

func TestOlderCyberPowerFirmwareIsNotWrittenTo(t *testing.T) {
	a := snmptest.New(t)
	a.SetInt("1.3.6.1.4.1.3808.1.1.3.1.8.0", 8)
	if err := namer(t, pdu.CyberPower, a).SetOutletName(context.Background(), 1, "200U1"); !errors.Is(err, pdu.ErrNoNaming) {
		t.Fatalf("%v", err)
	}
	if len(sets(a)) != 0 {
		t.Error("something was written to an unknown object")
	}
}

func TestAPCNamesGoToTheirConfigTables(t *testing.T) {
	// rPDU2, switched
	a := apcAgent(t)
	seed(a, apc+".26.9.2.1.1.3", 24)
	mirror(a, ".26.9.2.1.1.3.", ".26.9.2.3.1.3.")
	if err := namer(t, pdu.APC, a).SetOutletName(context.Background(), 5, "200U13"); err != nil {
		t.Fatal(err)
	}
	got := sets(a)
	if len(got) != 1 || got[0].VarBinds[0].OID.String() != apc+".26.9.2.1.1.3.5" || string(got[0].VarBinds[0].Value.Bytes) != "200U13" || got[0].Community != "private" {
		t.Fatalf("switched: %+v", got)
	}
	snap, err := newDriver(t, pdu.APC, a).Poll(context.Background())
	if err != nil || snap.Outlet[4].Name != "200U13" {
		t.Fatalf("%v %+v", err, snap.Outlet[4])
	}

	// rPDU2, metered only
	m := snmptest.New(t)
	m.SetInt(apc+".26.4.2.1.4.1", 8)
	m.SetInt(apc+".26.4.2.1.7.1", 1)
	m.SetInt(apc+".26.6.3.1.5.1", 24)
	for i := 1; i <= 8; i++ {
		m.SetString(fmt.Sprintf("%s.26.9.4.3.1.3.%d", apc, i), fmt.Sprintf("Outlet_%d", i))
		m.SetInt(fmt.Sprintf("%s.26.9.4.3.1.6.%d", apc, i), 0)
		m.SetInt(fmt.Sprintf("%s.26.9.4.3.1.7.%d", apc, i), 0)
	}
	seed(m, apc+".26.9.4.1.1.3", 8)
	if err := namer(t, pdu.APC, m).SetOutletName(context.Background(), 3, "N100U40"); err != nil {
		t.Fatal(err)
	}
	if got := sets(m); len(got) != 1 || got[0].VarBinds[0].OID.String() != apc+".26.9.4.1.1.3.3" {
		t.Fatalf("metered: %+v", got)
	}

	// the older rPDU / sPDU firmware
	l := snmptest.New(t)
	l.SetString(apc+".12.1.5.0", "AP7932")
	l.SetInt(apc+".12.1.8.0", 24)
	seed(l, apc+".12.3.4.1.1.2", 24)
	if err := namer(t, pdu.APC, l).SetOutletName(context.Background(), 2, "200U29"); err != nil {
		t.Fatal(err)
	}
	if got := sets(l); len(got) != 1 || got[0].VarBinds[0].OID.String() != apc+".12.3.4.1.1.2.2" {
		t.Fatalf("legacy: %+v", got)
	}
}

func TestBadNamesAreNeverSent(t *testing.T) {
	a := cyberPowerAgent(t)
	seed(a, cps+".6.1.2.1.4", 16)
	d := namer(t, pdu.CyberPower, a)
	for _, name := range []string{"", strings.Repeat("x", pdu.MaxOutletNameLen+1), "a\x00b", "a\nb", "naïve", "200U13\x7f"} {
		if err := d.SetOutletName(context.Background(), 1, name); err == nil {
			t.Errorf("%q was accepted", name)
		}
	}
	if err := d.SetOutletName(context.Background(), 1, strings.Repeat("x", pdu.MaxOutletNameLen)); err != nil {
		t.Errorf("the longest name: %v", err)
	}
	if len(sets(a)) != 1 {
		t.Errorf("%d writes, want only the valid one", len(sets(a)))
	}
}

func TestSimulatedPDUCanBeNamed(t *testing.T) {
	s := pdu.NewSimulated(pdu.CyberPower, "PDU", 4, true, nil)
	if err := s.SetOutletName(context.Background(), 2, "200U13"); err != nil {
		t.Fatal(err)
	}
	snap, _ := s.Poll(context.Background())
	if snap.Outlet[1].Name != "200U13" {
		t.Fatalf("%+v", snap.Outlet[1])
	}
	if s.SetOutletName(context.Background(), 9, "x") == nil || s.SetOutletName(context.Background(), 1, "") == nil {
		t.Error("a bad outlet or name")
	}
}
