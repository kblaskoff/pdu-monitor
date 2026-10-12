package snmp_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/kblaskoff/pdu-monitor/server/internal/snmp"
	"github.com/kblaskoff/pdu-monitor/server/internal/snmp/snmptest"
)

func hexOf(b []byte) string {
	parts := make([]string, len(b))
	for i, c := range b {
		parts[i] = fmt.Sprintf("%02x", c)
	}
	return strings.Join(parts, " ")
}

func TestGetRequestMatchesKnownBytes(t *testing.T) {
	// The same vector as the Swift tests of the Mac application (worked out by hand from the BER rules).
	m := &snmp.Message{Community: "public", Type: snmp.GetRequest, RequestID: 1,
		VarBinds: []snmp.VarBind{{OID: snmp.MustOID("1.3.6.1.2.1.1.1.0")}}}
	want := "30 26 02 01 00 04 06 70 75 62 6c 69 63 a0 19 02 01 01 02 01 00 02 01 00 30 0e 30 0c 06 08 2b 06 01 02 01 01 01 00 05 00"
	if got := hexOf(m.Marshal()); got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func TestRoundTrip(t *testing.T) {
	m := &snmp.Message{Community: "x", Type: snmp.GetResponse, RequestID: 123456, ErrorStatus: 2, ErrorIndex: 3, VarBinds: []snmp.VarBind{
		{OID: snmp.MustOID("1.3.6.1.2.1.1.1.0"), Value: snmp.OctetString("PDU81007")},
		{OID: snmp.MustOID("1.3.6.1.2.1.1.3.0"), Value: snmp.Unsigned(4_000_000_000)},
		{OID: snmp.MustOID("1.3.6.1.4.1.3808.1.1.6"), Value: snmp.Value{Kind: snmp.KindObjectID, OID: snmp.MustOID("1.3.6.1.4.1.3808")}},
		{OID: snmp.MustOID("1.3.6.1.2.1.2.1.0"), Value: snmp.Integer(-5)},
		{OID: snmp.MustOID("1.3.6.1.2.1.2.2.0"), Value: snmp.Integer(128)},
		{OID: snmp.MustOID("1.3.6.1.2.1.2.3.0"), Value: snmp.Value{}},
	}}
	got, err := snmp.Unmarshal(m.Marshal())
	if err != nil {
		t.Fatal(err)
	}
	if got.RequestID != 123456 || got.ErrorStatus != 2 || got.ErrorIndex != 3 || got.Community != "x" || len(got.VarBinds) != 6 {
		t.Fatalf("header: %+v", got)
	}
	if s, _ := got.VarBinds[0].Value.AsString(); s != "PDU81007" {
		t.Error(s)
	}
	if n, _ := got.VarBinds[1].Value.AsInt(); n != 4_000_000_000 {
		t.Error(n)
	}
	if got.VarBinds[2].Value.OID.String() != "1.3.6.1.4.1.3808" {
		t.Error(got.VarBinds[2].Value.OID)
	}
	if n, _ := got.VarBinds[3].Value.AsInt(); n != -5 {
		t.Error(n)
	}
	if n, _ := got.VarBinds[4].Value.AsInt(); n != 128 {
		t.Error(n)
	}
}

func TestOIDEncodingWithLargeSubidentifier(t *testing.T) {
	m := &snmp.Message{Community: "p", Type: snmp.GetRequest, VarBinds: []snmp.VarBind{{OID: snmp.MustOID("1.3.6.1.4.1.3808.1.1.6")}}}
	if !bytes.Contains(m.Marshal(), []byte{0x06, 0x0a, 0x2b, 0x06, 0x01, 0x04, 0x01, 0x9d, 0x60, 0x01, 0x01, 0x06}) {
		t.Fatalf("%s", hexOf(m.Marshal()))
	}
}

func TestLongLengths(t *testing.T) {
	text := strings.Repeat("a", 300)
	m := &snmp.Message{Community: "public", Type: snmp.GetResponse, VarBinds: []snmp.VarBind{{OID: snmp.MustOID("1.3.6"), Value: snmp.OctetString(text)}}}
	got, err := snmp.Unmarshal(m.Marshal())
	if err != nil {
		t.Fatal(err)
	}
	if s, _ := got.VarBinds[0].Value.AsString(); s != text {
		t.Fatal("text changed")
	}
}

func TestGarbageIsRejectedNotCrashing(t *testing.T) {
	for _, b := range [][]byte{nil, {}, {0x30, 0x05, 0x02}, {0x04, 0x01, 0x41}, {0x30, 0x84, 0xff, 0xff, 0xff, 0xff}, bytes.Repeat([]byte{0xff}, 40)} {
		if _, err := snmp.Unmarshal(b); err == nil {
			t.Errorf("accepted %x", b)
		}
	}
	// every prefix of a good message is damaged: none may panic
	good := (&snmp.Message{Community: "public", Type: snmp.GetResponse, RequestID: 7, VarBinds: []snmp.VarBind{{OID: snmp.MustOID("1.3.6.1.2.1.1.1.0"), Value: snmp.OctetString("hello")}}}).Marshal()
	for i := 0; i < len(good); i++ {
		if _, err := snmp.Unmarshal(good[:i]); err == nil {
			t.Errorf("accepted a message cut at %d", i)
		}
	}
}

func TestParseOID(t *testing.T) {
	if o, err := snmp.ParseOID(".1.3.6"); err != nil || o.String() != "1.3.6" {
		t.Error(o, err)
	}
	for _, bad := range []string{"", "1.x", "1..2", "-1"} {
		if _, err := snmp.ParseOID(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestClientGetSetAndCommunities(t *testing.T) {
	a := snmptest.New(t)
	a.SetString("1.3.6.1.2.1.1.1.0", "PDU")
	a.SetInt("1.3.6.1.2.1.1.7.0", 1)
	c := a.Client()
	binds, err := c.Get(context.Background(), []snmp.OID{snmp.MustOID("1.3.6.1.2.1.1.1.0")})
	if err != nil {
		t.Fatal(err)
	}
	if s, _ := binds[0].Value.AsString(); s != "PDU" {
		t.Fatal(s)
	}
	if err := c.Set(context.Background(), []snmp.VarBind{{OID: snmp.MustOID("1.3.6.1.2.1.1.7.0"), Value: snmp.Integer(2)}}); err != nil {
		t.Fatal(err)
	}
	if v, _ := a.Get("1.3.6.1.2.1.1.7.0"); v.Int != 2 {
		t.Fatal("not set")
	}
	sets := 0
	for _, r := range a.Requests() {
		if r.Type == snmp.SetRequest {
			sets++
			if r.Community != "private" {
				t.Errorf("SET used community %q", r.Community)
			}
		}
	}
	if sets != 1 {
		t.Fatal(sets)
	}
	// the read community cannot change anything: the agent stays silent
	ro := a.Client()
	ro.WriteCommunity = ""
	err = ro.Set(context.Background(), []snmp.VarBind{{OID: snmp.MustOID("1.3.6.1.2.1.1.7.0"), Value: snmp.Integer(3)}})
	if !errors.Is(err, snmp.ErrTimeout) {
		t.Fatalf("got %v", err)
	}
}

func TestNoAnswerIsATimeout(t *testing.T) {
	c := &snmp.UDPClient{Addr: "127.0.0.1:9", ReadCommunity: "public", Timeout: 300e6}
	if _, err := c.Get(context.Background(), []snmp.OID{snmp.MustOID("1.3.6.1.2.1.1.1.0")}); err == nil {
		t.Fatal("expected an error")
	}
}

func TestGetAvailableDropsMissingObjectsAndKeepsTheRest(t *testing.T) {
	a := snmptest.New(t)
	a.SetInt("1.3.6.1.2.1.1.1.0", 1)
	a.SetInt("1.3.6.1.2.1.1.3.0", 3)
	var oids []snmp.OID
	for _, s := range []string{"1.3.6.1.2.1.1.1.0", "1.3.6.1.2.1.1.2.0", "1.3.6.1.2.1.1.3.0", "1.3.6.1.2.1.1.9.0"} {
		oids = append(oids, snmp.MustOID(s))
	}
	got, err := snmp.GetAvailable(context.Background(), a.Client(), oids, 16)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("%v", got)
	}
	if n, _ := got.Int(snmp.MustOID("1.3.6.1.2.1.1.3.0")); n != 3 {
		t.Fatal(n)
	}
}

func TestGetAvailableSplitsRequestsOfADeviceWithASmallPacketLimit(t *testing.T) {
	a := snmptest.New(t)
	a.LimitVarBinds(3)
	var oids []snmp.OID
	for i := 1; i <= 25; i++ {
		s := fmt.Sprintf("1.3.6.1.4.1.9.%d", i)
		a.SetInt(s, int64(i))
		oids = append(oids, snmp.MustOID(s))
	}
	got, err := snmp.GetAvailable(context.Background(), a.Client(), oids, 16)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 25 {
		t.Fatalf("got %d of 25", len(got))
	}
	for i := 1; i <= 25; i++ {
		if n, _ := got.Int(snmp.MustOID(fmt.Sprintf("1.3.6.1.4.1.9.%d", i))); n != i {
			t.Errorf("object %d: %d", i, n)
		}
	}
}

func TestTooBigEvenForOneObjectIsReported(t *testing.T) {
	a := snmptest.New(t)
	a.SetInt("1.3.6.1.2.1.1.1.0", 1)
	a.LimitVarBinds(0)
	_, err := snmp.GetAvailable(context.Background(), a.Client(), []snmp.OID{snmp.MustOID("1.3.6.1.2.1.1.1.0")}, 8)
	if !errors.Is(err, snmp.ErrTooBig) {
		t.Fatalf("got %v", err)
	}
}
