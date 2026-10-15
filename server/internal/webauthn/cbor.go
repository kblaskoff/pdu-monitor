package webauthn

import (
	"encoding/binary"
	"errors"
	"math"
)

// A decoder for the small part of CBOR (RFC 8949) that WebAuthn uses: unsigned and negative integers, byte and text strings, arrays,
// maps, true/false/null. Nothing else is accepted: no floats, no tags, no indefinite lengths. Lengths are checked against what is
// left of the input before anything is allocated, and the depth is limited, so that input from a browser cannot make it do much.

const maxDepth = 6

var errCBOR = errors.New("the data is not valid CBOR")

// decodeCBOR returns the first item of b, and how many bytes it took.
func decodeCBOR(b []byte) (any, int, error) { return decodeItem(b, 0) }

func decodeItem(b []byte, depth int) (any, int, error) {
	if depth > maxDepth || len(b) == 0 {
		return nil, 0, errCBOR
	}
	major, info := b[0]>>5, b[0]&0x1f
	pos := 1
	var arg uint64
	switch {
	case info < 24:
		arg = uint64(info)
	case info == 24 && len(b) >= 2:
		arg, pos = uint64(b[1]), 2
	case info == 25 && len(b) >= 3:
		arg, pos = uint64(binary.BigEndian.Uint16(b[1:])), 3
	case info == 26 && len(b) >= 5:
		arg, pos = uint64(binary.BigEndian.Uint32(b[1:])), 5
	case info == 27 && len(b) >= 9:
		arg, pos = binary.BigEndian.Uint64(b[1:]), 9
	default:
		return nil, 0, errCBOR // an indefinite length, a reserved value, or the argument is cut off
	}
	rest := uint64(len(b) - pos)
	switch major {
	case 0:
		if arg > math.MaxInt64 {
			return nil, 0, errCBOR
		}
		return int64(arg), pos, nil
	case 1:
		if arg > math.MaxInt64 {
			return nil, 0, errCBOR
		}
		return -1 - int64(arg), pos, nil
	case 2, 3:
		if arg > rest {
			return nil, 0, errCBOR
		}
		data := b[pos : pos+int(arg)]
		if major == 3 {
			return string(data), pos + int(arg), nil
		}
		return append([]byte(nil), data...), pos + int(arg), nil
	case 4:
		if arg > rest { // every item takes at least one byte
			return nil, 0, errCBOR
		}
		list := make([]any, 0, arg)
		for i := uint64(0); i < arg; i++ {
			v, n, err := decodeItem(b[pos:], depth+1)
			if err != nil {
				return nil, 0, err
			}
			list, pos = append(list, v), pos+n
		}
		return list, pos, nil
	case 5:
		if arg > rest/2 {
			return nil, 0, errCBOR
		}
		m := make(map[any]any, arg)
		for i := uint64(0); i < arg; i++ {
			k, n, err := decodeItem(b[pos:], depth+1)
			if err != nil {
				return nil, 0, err
			}
			pos += n
			switch k.(type) {
			case int64, string:
			default:
				return nil, 0, errCBOR
			}
			v, n, err := decodeItem(b[pos:], depth+1)
			if err != nil {
				return nil, 0, err
			}
			pos += n
			if _, dup := m[k]; dup {
				return nil, 0, errCBOR
			}
			m[k] = v
		}
		return m, pos, nil
	case 7:
		switch info {
		case 20:
			return false, 1, nil
		case 21:
			return true, 1, nil
		case 22:
			return nil, 1, nil
		}
	}
	return nil, 0, errCBOR
}
