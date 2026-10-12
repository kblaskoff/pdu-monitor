package snmp

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"sync/atomic"
	"time"
)

// Errors of the agent.
var (
	ErrTimeout = errors.New("no answer (timeout)")
	// ErrTooBig is SNMPv1 error-status 1: the answer would not fit in a packet the device is willing to send.
	ErrTooBig = errors.New("the device cannot answer such a big request (tooBig)")
)

// NoSuchNameError is error-status 2: one of the requested objects does not exist (Index is 1-based, 0 when the agent does not say which).
type NoSuchNameError struct{ Index int }

func (e *NoSuchNameError) Error() string { return "the device does not have this object" }

// AgentError is any other error-status.
type AgentError struct{ Status, Index int }

func (e *AgentError) Error() string {
	switch e.Status {
	case 3:
		return "the device refused the value (badValue)"
	case 4:
		return "the device refused the change: the community is read-only (readOnly)"
	case 5:
		return "the device reported a general error (genErr)"
	}
	return fmt.Sprintf("the device answered with SNMP error %d", e.Status)
}

// Transport is what the PDU drivers need. UDPClient is the real one; the tests use fakes.
type Transport interface {
	// Get asks for all the OIDs in one request.
	Get(ctx context.Context, oids []OID) ([]VarBind, error)
	// Set changes values (uses the write community).
	Set(ctx context.Context, binds []VarBind) error
}

// UDPClient talks SNMPv1 to one device.
type UDPClient struct {
	Addr           string // host:port
	ReadCommunity  string
	WriteCommunity string // the read community when empty
	Timeout        time.Duration
	Retries        int
	// Debug, when set, gets one line for every request and answer (communities are never written).
	Debug func(string)
}

var requestCounter atomic.Int32

func init() { requestCounter.Store(rand.Int31n(1_000_000) + 1) }

func nextID() int32 {
	for {
		v := requestCounter.Add(1)
		if v > 0 {
			return v
		}
		requestCounter.Store(1)
	}
}

func (c *UDPClient) debugf(format string, args ...any) {
	if c.Debug != nil {
		c.Debug(fmt.Sprintf(format, args...))
	}
}

func (c *UDPClient) Get(ctx context.Context, oids []OID) ([]VarBind, error) {
	msg := &Message{Community: c.ReadCommunity, Type: GetRequest, RequestID: nextID()}
	for _, o := range oids {
		msg.VarBinds = append(msg.VarBinds, VarBind{OID: o})
	}
	answer, err := c.exchange(ctx, msg)
	if err != nil {
		return nil, err
	}
	return answer.VarBinds, nil
}

func (c *UDPClient) Set(ctx context.Context, binds []VarBind) error {
	community := c.WriteCommunity
	if community == "" {
		community = c.ReadCommunity
	}
	_, err := c.exchange(ctx, &Message{Community: community, Type: SetRequest, RequestID: nextID(), VarBinds: binds})
	return err
}

func (c *UDPClient) exchange(ctx context.Context, msg *Message) (*Message, error) {
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "udp", c.Addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	packet := msg.Marshal()
	buf := make([]byte, 65535)
	for attempt := 0; attempt <= c.Retries; attempt++ {
		if _, err := conn.Write(packet); err != nil {
			return nil, err
		}
		deadline := time.Now().Add(timeout)
		if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
			deadline = d
		}
		for {
			_ = conn.SetReadDeadline(deadline)
			n, err := conn.Read(buf)
			if err != nil {
				var ne net.Error
				if errors.As(err, &ne) && ne.Timeout() {
					c.debugf("%s %#x %d object(s): no answer within %s", c.Addr, byte(msg.Type), len(msg.VarBinds), timeout)
					break
				}
				return nil, err // for example "connection refused" after an ICMP port unreachable
			}
			answer, err := Unmarshal(buf[:n])
			if err != nil || answer.Type != GetResponse || answer.RequestID != msg.RequestID {
				continue // not ours: wait on
			}
			c.debugf("%s %#x %d object(s) -> status %d index %d, %d answer(s)", c.Addr, byte(msg.Type), len(msg.VarBinds), answer.ErrorStatus, answer.ErrorIndex, len(answer.VarBinds))
			switch answer.ErrorStatus {
			case 0:
				return answer, nil
			case 1:
				return nil, ErrTooBig
			case 2:
				return nil, &NoSuchNameError{Index: answer.ErrorIndex}
			default:
				return nil, &AgentError{Status: answer.ErrorStatus, Index: answer.ErrorIndex}
			}
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	return nil, ErrTimeout
}

// Values are the answers of a GetAvailable, by OID.
type Values map[string]Value

func (v Values) Int(o OID) (int, bool) {
	if val, ok := v[o.String()]; ok {
		return val.AsInt()
	}
	return 0, false
}

// Tenths is a number given in tenths (amps, volts). Negative means "not supported" for some APC objects.
func (v Values) Tenths(o OID) (float64, bool) {
	n, ok := v.Int(o)
	if !ok || n < 0 {
		return 0, false
	}
	return float64(n) / 10, true
}

func (v Values) String(o OID) (string, bool) {
	if val, ok := v[o.String()]; ok {
		s, ok := val.AsString()
		return s, ok && s != ""
	}
	return "", false
}

func (v Values) Has(o OID) bool { _, ok := v[o.String()]; return ok }

// GetAvailable asks for many objects and survives the ones the device does not have: the offending OID is dropped and the rest
// is asked again. A device that says "too big" gets the same objects in two halves. Only the OIDs the device has are returned.
func GetAvailable(ctx context.Context, t Transport, oids []OID, chunk int) (Values, error) {
	if chunk < 1 {
		chunk = 8
	}
	result := Values{}
	var queue [][]OID
	for i := len(oids); i > 0; i -= chunk { // reversed, so that the queue can be used as a stack
		lo := i - chunk
		if lo < 0 {
			lo = 0
		}
		queue = append(queue, oids[lo:i])
	}
	for len(queue) > 0 {
		pending := append([]OID(nil), queue[len(queue)-1]...)
		queue = queue[:len(queue)-1]
		for len(pending) > 0 {
			binds, err := t.Get(ctx, pending)
			if err == nil {
				for _, vb := range binds {
					if !vb.Value.Missing() {
						result[vb.OID.String()] = vb.Value
					}
				}
				break
			}
			var nsn *NoSuchNameError
			switch {
			case errors.Is(err, ErrTooBig):
				if len(pending) == 1 {
					return nil, err
				}
				half := len(pending) / 2
				queue = append(queue, append([]OID(nil), pending[half:]...))
				pending = pending[:half]
			case errors.As(err, &nsn):
				if nsn.Index >= 1 && nsn.Index <= len(pending) {
					pending = append(pending[:nsn.Index-1], pending[nsn.Index:]...)
					continue
				}
				// the agent does not say which one: every object is asked alone
				for _, o := range pending {
					binds, err := t.Get(ctx, []OID{o})
					var one *NoSuchNameError
					if errors.As(err, &one) {
						continue
					}
					if err != nil {
						return nil, err
					}
					for _, vb := range binds {
						if !vb.Value.Missing() {
							result[vb.OID.String()] = vb.Value
						}
					}
				}
				pending = nil
			default:
				return nil, err
			}
		}
	}
	return result, nil
}
