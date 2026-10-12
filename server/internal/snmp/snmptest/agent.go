// Package snmptest is an SNMPv1 agent on 127.0.0.1 with a table of values, for tests of the client and the PDU drivers
// over real UDP.
package snmptest

import (
	"net"
	"sort"
	"sync"
	"testing"

	"github.com/kblaskoff/pdu-monitor/server/internal/snmp"
)

// Agent answers GET and SET like a PDU: unknown objects give noSuchName with the index of the first one.
type Agent struct {
	Read, Write string
	conn        *net.UDPConn
	mu          sync.Mutex
	values      map[string]snmp.Value
	oids        map[string]snmp.OID
	requests    []*snmp.Message
	maxVarBinds int
	onSet       func(a *Agent, oid snmp.OID, v snmp.Value)
	done        chan struct{}
}

// New starts an agent; it stops when the test ends.
func New(t testing.TB) *Agent {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	a := &Agent{Read: "public", Write: "private", conn: conn, values: map[string]snmp.Value{}, oids: map[string]snmp.OID{},
		maxVarBinds: 1 << 30, done: make(chan struct{})}
	go a.loop()
	t.Cleanup(func() { conn.Close(); <-a.done })
	return a
}

// LimitVarBinds makes the agent answer a request with more objects than n with error-status 1 (tooBig), like PDUs with a small
// packet limit do.
func (a *Agent) LimitVarBinds(n int) {
	a.mu.Lock()
	a.maxVarBinds = n
	a.mu.Unlock()
}

// OnSet registers a function that is called after a SET, so that a test can mirror a control object into a status object.
func (a *Agent) OnSet(f func(a *Agent, oid snmp.OID, v snmp.Value)) {
	a.mu.Lock()
	a.onSet = f
	a.mu.Unlock()
}

// Addr is host:port.
func (a *Agent) Addr() string { return a.conn.LocalAddr().String() }

// Client returns a UDP client for this agent (short timeouts).
func (a *Agent) Client() *snmp.UDPClient {
	return &snmp.UDPClient{Addr: a.Addr(), ReadCommunity: a.Read, WriteCommunity: a.Write, Timeout: 700e6, Retries: 0}
}

// Set stores a value; the OID is a string such as "1.3.6.1.2.1.1.1.0".
func (a *Agent) Set(oid string, v snmp.Value) {
	o := snmp.MustOID(oid)
	a.mu.Lock()
	defer a.mu.Unlock()
	a.values[o.String()] = v
	a.oids[o.String()] = o
}

func (a *Agent) SetInt(oid string, n int64)     { a.Set(oid, snmp.Integer(n)) }
func (a *Agent) SetUint(oid string, n uint64)   { a.Set(oid, snmp.Unsigned(n)) }
func (a *Agent) SetString(oid string, s string) { a.Set(oid, snmp.OctetString(s)) }

// Get reads a stored value.
func (a *Agent) Get(oid string) (snmp.Value, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	v, ok := a.values[snmp.MustOID(oid).String()]
	return v, ok
}

// Requests are the messages received so far.
func (a *Agent) Requests() []*snmp.Message {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]*snmp.Message(nil), a.requests...)
}

func (a *Agent) loop() {
	defer close(a.done)
	buf := make([]byte, 65535)
	for {
		n, from, err := a.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		req, err := snmp.Unmarshal(buf[:n])
		if err != nil {
			continue
		}
		a.mu.Lock()
		a.requests = append(a.requests, req)
		a.mu.Unlock()
		if answer := a.respond(req); answer != nil {
			_, _ = a.conn.WriteToUDP(answer.Marshal(), from)
		}
	}
}

func (a *Agent) respond(req *snmp.Message) *snmp.Message {
	want := a.Read
	if req.Type == snmp.SetRequest {
		want = a.Write
	}
	if req.Community != want && !(req.Type == snmp.GetRequest && req.Community == a.Write) {
		return nil // silently dropped, like a real agent
	}
	ans := &snmp.Message{Community: req.Community, Type: snmp.GetResponse, RequestID: req.RequestID, VarBinds: append([]snmp.VarBind(nil), req.VarBinds...)}
	a.mu.Lock()
	limit, onSet := a.maxVarBinds, a.onSet
	a.mu.Unlock()
	if len(req.VarBinds) > limit {
		ans.ErrorStatus = 1
		return ans
	}
	switch req.Type {
	case snmp.GetRequest:
		for i, vb := range req.VarBinds {
			v, ok := a.Get(vb.OID.String())
			if !ok {
				ans.ErrorStatus, ans.ErrorIndex = 2, i+1
				return ans
			}
			ans.VarBinds[i].Value = v
		}
	case snmp.GetNextRequest:
		a.mu.Lock()
		keys := make([]string, 0, len(a.oids))
		for k := range a.oids {
			keys = append(keys, k)
		}
		a.mu.Unlock()
		sort.Slice(keys, func(i, j int) bool { return less(a.oids[keys[i]], a.oids[keys[j]]) })
		for _, k := range keys {
			if less(req.VarBinds[0].OID, a.oids[k]) {
				v, _ := a.Get(k)
				ans.VarBinds = []snmp.VarBind{{OID: a.oids[k], Value: v}}
				return ans
			}
		}
		ans.ErrorStatus, ans.ErrorIndex = 2, 1
	case snmp.SetRequest:
		for _, vb := range req.VarBinds {
			if _, ok := a.Get(vb.OID.String()); !ok {
				ans.ErrorStatus, ans.ErrorIndex = 2, 1
				return ans
			}
			a.mu.Lock()
			a.values[vb.OID.String()] = vb.Value
			a.mu.Unlock()
			if onSet != nil {
				onSet(a, vb.OID, vb.Value)
			}
		}
	default:
		return nil
	}
	return ans
}

func less(a, b snmp.OID) bool {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return len(a) < len(b)
}
