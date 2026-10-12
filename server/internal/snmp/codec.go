// Package snmp is a small SNMPv1 client: GET and SET over UDP, BER encoding and decoding. Nothing else is needed for PDUs.
package snmp

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// OID is an object identifier such as 1.3.6.1.4.1.3808.1.1.6.
type OID []uint32

// ParseOID reads "1.3.6.1.2.1" (a leading dot is accepted).
func ParseOID(s string) (OID, error) {
	parts := strings.Split(strings.TrimPrefix(s, "."), ".")
	if len(parts) == 0 || parts[0] == "" {
		return nil, errors.New("empty OID")
	}
	oid := make(OID, 0, len(parts))
	for _, p := range parts {
		n, err := strconv.ParseUint(p, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("bad OID %q", s)
		}
		oid = append(oid, uint32(n))
	}
	return oid, nil
}

// MustOID is for constants in the program: it panics on a typing mistake.
func MustOID(s string) OID {
	oid, err := ParseOID(s)
	if err != nil {
		panic(err)
	}
	return oid
}

func (o OID) String() string {
	parts := make([]string, len(o))
	for i, n := range o {
		parts[i] = strconv.FormatUint(uint64(n), 10)
	}
	return strings.Join(parts, ".")
}

// Append returns a new OID with the numbers added (the receiver is not changed).
func (o OID) Append(more ...uint32) OID {
	out := make(OID, 0, len(o)+len(more))
	out = append(out, o...)
	return append(out, more...)
}

// Kind is the type of a value.
type Kind int

const (
	KindNull Kind = iota
	KindInteger
	KindUnsigned // Gauge32, Counter32, TimeTicks, Counter64
	KindOctetString
	KindObjectID
	KindIPAddress
	KindNoSuchObject
	KindNoSuchInstance
	KindEndOfMibView
)

// Value is one SNMP value.
type Value struct {
	Kind  Kind
	Int   int64
	Uint  uint64
	Bytes []byte
	OID   OID
}

func Integer(n int64) Value      { return Value{Kind: KindInteger, Int: n} }
func Unsigned(n uint64) Value    { return Value{Kind: KindUnsigned, Uint: n} }
func OctetString(s string) Value { return Value{Kind: KindOctetString, Bytes: []byte(s)} }
func (v Value) Missing() bool    { return v.Kind >= KindNoSuchObject }

// AsInt returns a number for integer and unsigned values.
func (v Value) AsInt() (int, bool) {
	switch v.Kind {
	case KindInteger:
		return int(v.Int), true
	case KindUnsigned:
		if v.Uint > 1<<62 {
			return 0, false
		}
		return int(v.Uint), true
	}
	return 0, false
}

// AsString returns the text of an octet string without the padding some agents add.
func (v Value) AsString() (string, bool) {
	if v.Kind != KindOctetString {
		return "", false
	}
	return strings.Trim(string(v.Bytes), "\x00 \t\r\n"), true
}

// VarBind is an OID with its value.
type VarBind struct {
	OID   OID
	Value Value
}

// PDUType is the kind of request.
type PDUType byte

const (
	GetRequest     PDUType = 0xA0
	GetNextRequest PDUType = 0xA1
	GetResponse    PDUType = 0xA2
	SetRequest     PDUType = 0xA3
)

// Message is one SNMPv1 message.
type Message struct {
	Community   string
	Type        PDUType
	RequestID   int32
	ErrorStatus int
	ErrorIndex  int
	VarBinds    []VarBind
}

// ---- encoding

func berLength(n int) []byte {
	if n < 0x80 {
		return []byte{byte(n)}
	}
	var b []byte
	for rest := n; rest > 0; rest >>= 8 {
		b = append([]byte{byte(rest & 0xFF)}, b...)
	}
	return append([]byte{0x80 | byte(len(b))}, b...)
}

func tlv(tag byte, content []byte) []byte {
	out := []byte{tag}
	out = append(out, berLength(len(content))...)
	return append(out, content...)
}

func berInteger(v int64) []byte {
	var b []byte
	for {
		b = append([]byte{byte(v)}, b...)
		v >>= 8
		if (v == 0 && b[0]&0x80 == 0) || (v == -1 && b[0]&0x80 != 0) {
			break
		}
	}
	return tlv(0x02, b)
}

func berUnsigned(tag byte, v uint64) []byte {
	var b []byte
	for {
		b = append([]byte{byte(v)}, b...)
		v >>= 8
		if v == 0 {
			break
		}
	}
	if b[0]&0x80 != 0 {
		b = append([]byte{0}, b...)
	}
	return tlv(tag, b)
}

func berOID(o OID) []byte {
	var b []byte
	switch {
	case len(o) >= 2:
		b = append(b, byte(o[0]*40+o[1]))
		for _, sub := range o[2:] {
			chunk := []byte{byte(sub & 0x7F)}
			for rest := sub >> 7; rest > 0; rest >>= 7 {
				chunk = append([]byte{byte(rest&0x7F) | 0x80}, chunk...)
			}
			b = append(b, chunk...)
		}
	case len(o) == 1:
		b = append(b, byte(o[0]*40))
	}
	return tlv(0x06, b)
}

func berValue(v Value) []byte {
	switch v.Kind {
	case KindInteger:
		return berInteger(v.Int)
	case KindUnsigned:
		return berUnsigned(0x42, v.Uint)
	case KindOctetString:
		return tlv(0x04, v.Bytes)
	case KindObjectID:
		return berOID(v.OID)
	case KindIPAddress:
		return tlv(0x40, v.Bytes)
	case KindNoSuchObject:
		return []byte{0x80, 0}
	case KindNoSuchInstance:
		return []byte{0x81, 0}
	case KindEndOfMibView:
		return []byte{0x82, 0}
	}
	return []byte{0x05, 0x00}
}

// Marshal encodes the message.
func (m *Message) Marshal() []byte {
	var binds []byte
	for _, vb := range m.VarBinds {
		binds = append(binds, tlv(0x30, append(berOID(vb.OID), berValue(vb.Value)...))...)
	}
	pdu := append(berInteger(int64(m.RequestID)), berInteger(int64(m.ErrorStatus))...)
	pdu = append(pdu, berInteger(int64(m.ErrorIndex))...)
	pdu = append(pdu, tlv(0x30, binds)...)
	body := append(berInteger(0), tlv(0x04, []byte(m.Community))...)
	body = append(body, tlv(byte(m.Type), pdu)...)
	return tlv(0x30, body)
}

// ---- decoding

var errTruncated = errors.New("snmp: truncated message")

type reader struct {
	b   []byte
	pos int
}

func (r *reader) atEnd() bool { return r.pos >= len(r.b) }

func (r *reader) byte() (byte, error) {
	if r.pos >= len(r.b) {
		return 0, errTruncated
	}
	c := r.b[r.pos]
	r.pos++
	return c, nil
}

func (r *reader) length() (int, error) {
	first, err := r.byte()
	if err != nil {
		return 0, err
	}
	if first < 0x80 {
		return int(first), nil
	}
	n := int(first & 0x7F)
	if n == 0 || n > 4 {
		return 0, errors.New("snmp: bad length")
	}
	length := 0
	for i := 0; i < n; i++ {
		c, err := r.byte()
		if err != nil {
			return 0, err
		}
		length = length<<8 | int(c)
	}
	return length, nil
}

func (r *reader) tlv() (byte, []byte, error) {
	tag, err := r.byte()
	if err != nil {
		return 0, nil, err
	}
	length, err := r.length()
	if err != nil {
		return 0, nil, err
	}
	if length < 0 || r.pos+length > len(r.b) {
		return 0, nil, errTruncated
	}
	content := r.b[r.pos : r.pos+length]
	r.pos += length
	return tag, content, nil
}

func decodeInteger(b []byte) (int64, error) {
	if len(b) == 0 || len(b) > 8 {
		return 0, errors.New("snmp: bad integer")
	}
	var v int64
	if b[0]&0x80 != 0 {
		v = -1
	}
	for _, c := range b {
		v = v<<8 | int64(c)
	}
	return v, nil
}

func decodeUnsigned(b []byte) (uint64, error) {
	for len(b) > 1 && b[0] == 0 {
		b = b[1:]
	}
	if len(b) == 0 || len(b) > 8 {
		return 0, errors.New("snmp: bad unsigned")
	}
	var v uint64
	for _, c := range b {
		v = v<<8 | uint64(c)
	}
	return v, nil
}

func decodeOID(b []byte) (OID, error) {
	if len(b) == 0 {
		return nil, errors.New("snmp: empty OID")
	}
	var oid OID
	if b[0] < 80 {
		oid = OID{uint32(b[0] / 40), uint32(b[0] % 40)}
	} else {
		oid = OID{2, uint32(b[0]) - 80}
	}
	var cur uint32
	pending := false
	for _, c := range b[1:] {
		if cur > (1<<32-1)>>7 {
			return nil, errors.New("snmp: OID component too large")
		}
		cur = cur<<7 | uint32(c&0x7F)
		if c&0x80 == 0 {
			oid = append(oid, cur)
			cur, pending = 0, false
		} else {
			pending = true
		}
	}
	if pending {
		return nil, errors.New("snmp: bad OID")
	}
	return oid, nil
}

func decodeValue(tag byte, c []byte) (Value, error) {
	switch tag {
	case 0x02:
		n, err := decodeInteger(c)
		return Integer(n), err
	case 0x04, 0x44:
		return Value{Kind: KindOctetString, Bytes: append([]byte(nil), c...)}, nil
	case 0x05:
		return Value{}, nil
	case 0x06:
		o, err := decodeOID(c)
		return Value{Kind: KindObjectID, OID: o}, err
	case 0x40:
		return Value{Kind: KindIPAddress, Bytes: append([]byte(nil), c...)}, nil
	case 0x41, 0x42, 0x43, 0x46:
		n, err := decodeUnsigned(c)
		return Unsigned(n), err
	case 0x80:
		return Value{Kind: KindNoSuchObject}, nil
	case 0x81:
		return Value{Kind: KindNoSuchInstance}, nil
	case 0x82:
		return Value{Kind: KindEndOfMibView}, nil
	}
	return Value{}, fmt.Errorf("snmp: unknown value tag %#x", tag)
}

// Unmarshal decodes a message. Damaged input gives an error, never a panic.
func Unmarshal(data []byte) (*Message, error) {
	outer := &reader{b: data}
	tag, body, err := outer.tlv()
	if err != nil {
		return nil, err
	}
	if tag != 0x30 {
		return nil, errors.New("snmp: not a message")
	}
	r := &reader{b: body}
	tag, c, err := r.tlv()
	if err != nil || tag != 0x02 {
		return nil, errors.New("snmp: bad version")
	}
	if version, err := decodeInteger(c); err != nil || version != 0 {
		return nil, errors.New("snmp: only version 1 is supported")
	}
	tag, c, err = r.tlv()
	if err != nil || tag != 0x04 {
		return nil, errors.New("snmp: bad community")
	}
	m := &Message{Community: string(c)}
	tag, c, err = r.tlv()
	if err != nil {
		return nil, err
	}
	switch PDUType(tag) {
	case GetRequest, GetNextRequest, GetResponse, SetRequest:
		m.Type = PDUType(tag)
	default:
		return nil, fmt.Errorf("snmp: unsupported PDU %#x", tag)
	}
	p := &reader{b: c}
	var nums [3]int64
	for i := range nums {
		tag, c, err := p.tlv()
		if err != nil || tag != 0x02 {
			return nil, errors.New("snmp: bad PDU header")
		}
		if nums[i], err = decodeInteger(c); err != nil {
			return nil, err
		}
	}
	m.RequestID, m.ErrorStatus, m.ErrorIndex = int32(nums[0]), int(nums[1]), int(nums[2])
	tag, c, err = p.tlv()
	if err != nil || tag != 0x30 {
		return nil, errors.New("snmp: bad varbind list")
	}
	list := &reader{b: c}
	for !list.atEnd() {
		tag, entry, err := list.tlv()
		if err != nil || tag != 0x30 {
			return nil, errors.New("snmp: bad varbind")
		}
		er := &reader{b: entry}
		ntag, nc, err := er.tlv()
		if err != nil || ntag != 0x06 {
			return nil, errors.New("snmp: bad varbind name")
		}
		oid, err := decodeOID(nc)
		if err != nil {
			return nil, err
		}
		vtag, vc, err := er.tlv()
		if err != nil {
			return nil, err
		}
		value, err := decodeValue(vtag, vc)
		if err != nil {
			return nil, err
		}
		m.VarBinds = append(m.VarBinds, VarBind{OID: oid, Value: value})
	}
	return m, nil
}
