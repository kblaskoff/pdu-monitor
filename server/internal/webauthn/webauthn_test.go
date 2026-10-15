package webauthn

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"strings"
	"testing"
)

// ---- a software authenticator: what a browser and a laptop would send

func cborHead(major byte, n int) []byte {
	switch {
	case n < 24:
		return []byte{major<<5 | byte(n)}
	case n < 256:
		return []byte{major<<5 | 24, byte(n)}
	default:
		return []byte{major<<5 | 25, byte(n >> 8), byte(n)}
	}
}
func cInt(n int64) []byte {
	if n >= 0 {
		return cborHead(0, int(n))
	}
	return cborHead(1, int(-1-n))
}
func cBytes(b []byte) []byte { return append(cborHead(2, len(b)), b...) }
func cText(s string) []byte  { return append(cborHead(3, len(s)), s...) }
func cMap(kv ...[]byte) []byte {
	out := cborHead(5, len(kv)/2)
	for _, x := range kv {
		out = append(out, x...)
	}
	return out
}

type authenticator struct {
	t      *testing.T
	kind   string // es256, rs256, eddsa
	ec     *ecdsa.PrivateKey
	rsa    *rsa.PrivateKey
	ed     ed25519.PrivateKey
	credID []byte
	count  uint32
}

func newAuthenticator(t *testing.T, kind string) *authenticator {
	a := &authenticator{t: t, kind: kind, credID: []byte("credential-id-0123456789")}
	var err error
	switch kind {
	case "es256":
		a.ec, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	case "rs256":
		a.rsa, err = rsa.GenerateKey(rand.Reader, 2048)
	case "eddsa":
		_, a.ed, err = ed25519.GenerateKey(rand.Reader)
	}
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func pad32(b []byte) []byte { return append(make([]byte, 32-len(b)), b...) }

func (a *authenticator) cose() []byte {
	switch a.kind {
	case "es256":
		return cMap(cInt(1), cInt(2), cInt(3), cInt(-7), cInt(-1), cInt(1), cInt(-2), cBytes(pad32(a.ec.X.Bytes())), cInt(-3), cBytes(pad32(a.ec.Y.Bytes())))
	case "rs256":
		return cMap(cInt(1), cInt(3), cInt(3), cInt(-257), cInt(-1), cBytes(a.rsa.N.Bytes()), cInt(-2), cBytes([]byte{1, 0, 1}))
	}
	return cMap(cInt(1), cInt(1), cInt(3), cInt(-8), cInt(-1), cInt(6), cInt(-2), cBytes(a.ed.Public().(ed25519.PublicKey)))
}

func buildAuthData(rpID string, flags byte, count uint32, attested []byte) []byte {
	h := sha256.Sum256([]byte(rpID))
	out := append(h[:], flags)
	out = binary.BigEndian.AppendUint32(out, count)
	return append(out, attested...)
}

func (a *authenticator) attested() []byte {
	out := make([]byte, 16) // the aaguid
	out = binary.BigEndian.AppendUint16(out, uint16(len(a.credID)))
	out = append(out, a.credID...)
	return append(out, a.cose()...)
}

func clientJSON(typ string, challenge []byte, origin string) []byte {
	b, _ := json.Marshal(map[string]any{"type": typ, "challenge": B64(challenge), "origin": origin, "crossOrigin": false})
	return b
}

func attObj(authData []byte) []byte {
	return cMap(cText("fmt"), cText("none"), cText("attStmt"), cMap(), cText("authData"), cBytes(authData))
}

func (a *authenticator) sign(msg []byte) []byte {
	switch a.kind {
	case "es256":
		h := sha256.Sum256(msg)
		sig, err := ecdsa.SignASN1(rand.Reader, a.ec, h[:])
		if err != nil {
			a.t.Fatal(err)
		}
		return sig
	case "rs256":
		h := sha256.Sum256(msg)
		sig, err := rsa.SignPKCS1v15(rand.Reader, a.rsa, crypto.SHA256, h[:])
		if err != nil {
			a.t.Fatal(err)
		}
		return sig
	}
	return ed25519.Sign(a.ed, msg)
}

// assertion returns clientDataJSON, authenticatorData and the signature
func (a *authenticator) assertion(rpID, origin string, challenge []byte, flags byte, count uint32) (cd, ad, sig []byte) {
	cd = clientJSON("webauthn.get", challenge, origin)
	ad = buildAuthData(rpID, flags, count, nil)
	h := sha256.Sum256(cd)
	return cd, ad, a.sign(append(append([]byte(nil), ad...), h[:]...))
}

var rp = RP{ID: "pdu.example.com", Origins: []string{"https://pdu.example.com:8443"}}

const origin = "https://pdu.example.com:8443"

func TestRegistrationAndLoginWithEveryKeyType(t *testing.T) {
	for _, kind := range []string{"es256", "rs256", "eddsa"} {
		t.Run(kind, func(t *testing.T) {
			a := newAuthenticator(t, kind)
			chal := NewChallenge()
			reg, err := rp.ParseRegistration(chal, clientJSON("webauthn.create", chal, origin), attObj(buildAuthData(rp.ID, flagUP|flagUV|flagAT, 0, a.attested())))
			if err != nil {
				t.Fatal(err)
			}
			if string(reg.CredentialID) != string(a.credID) || len(reg.PublicKey) == 0 {
				t.Fatalf("%+v", reg)
			}
			chal = NewChallenge()
			cd, ad, sig := a.assertion(rp.ID, origin, chal, flagUP|flagUV, 0)
			if n, err := rp.VerifyAssertion(chal, reg.PublicKey, reg.SignCount, cd, ad, sig); err != nil || n != 0 {
				t.Fatalf("%d %v", n, err)
			}
		})
	}
}

func TestAssertionIsRefusedWhenAnythingIsWrong(t *testing.T) {
	a := newAuthenticator(t, "es256")
	chal := NewChallenge()
	reg, err := rp.ParseRegistration(chal, clientJSON("webauthn.create", chal, origin), attObj(buildAuthData(rp.ID, flagUP|flagUV|flagAT, 0, a.attested())))
	if err != nil {
		t.Fatal(err)
	}
	other := newAuthenticator(t, "es256")
	chal = NewChallenge()
	good := func() ([]byte, []byte, []byte) { return a.assertion(rp.ID, origin, chal, flagUP|flagUV, 0) }
	cd, ad, sig := good()
	if _, err := rp.VerifyAssertion(chal, reg.PublicKey, 0, cd, ad, sig); err != nil {
		t.Fatal(err)
	}
	type tc struct {
		name string
		run  func() error
		want string
	}
	verify := func(chal2 []byte, key []byte, count uint32, cd, ad, sig []byte) error {
		_, err := rp.VerifyAssertion(chal2, key, count, cd, ad, sig)
		return err
	}
	for _, c := range []tc{
		{"another challenge", func() error { return verify(NewChallenge(), reg.PublicKey, 0, cd, ad, sig) }, "challenge"},
		{"no challenge", func() error { return verify(nil, reg.PublicKey, 0, cd, ad, sig) }, "challenge"},
		{"another origin", func() error {
			c, d, s := a.assertion(rp.ID, "https://evil.example.com", chal, flagUP|flagUV, 0)
			return verify(chal, reg.PublicKey, 0, c, d, s)
		}, "another address"},
		{"another site", func() error {
			c, d, s := a.assertion("evil.example.com", origin, chal, flagUP|flagUV, 0)
			return verify(chal, reg.PublicKey, 0, c, d, s)
		}, "another site"},
		{"the person was not present", func() error {
			c, d, s := a.assertion(rp.ID, origin, chal, flagUV, 0)
			return verify(chal, reg.PublicKey, 0, c, d, s)
		}, "not present"},
		{"the person was not verified", func() error {
			c, d, s := a.assertion(rp.ID, origin, chal, flagUP, 0)
			return verify(chal, reg.PublicKey, 0, c, d, s)
		}, "not verified"},
		{"a key that is not the stored one", func() error {
			c, d, s := other.assertion(rp.ID, origin, chal, flagUP|flagUV, 0)
			return verify(chal, reg.PublicKey, 0, c, d, s)
		}, "signature"},
		{"a changed signature", func() error {
			s := append([]byte(nil), sig...)
			s[len(s)-1] ^= 1
			return verify(chal, reg.PublicKey, 0, cd, ad, s)
		}, "signature"},
		{"changed authenticator data", func() error {
			d := append([]byte(nil), ad...)
			d[36] ^= 1 // the counter
			return verify(chal, reg.PublicKey, 0, cd, d, sig)
		}, "signature"},
		{"the registration type", func() error {
			c := clientJSON("webauthn.create", chal, origin)
			h := sha256.Sum256(c)
			return verify(chal, reg.PublicKey, 0, c, ad, a.sign(append(append([]byte(nil), ad...), h[:]...)))
		}, "something else"},
		{"a counter that went back", func() error {
			c, d, s := a.assertion(rp.ID, origin, chal, flagUP|flagUV, 5)
			return verify(chal, reg.PublicKey, 9, c, d, s)
		}, "counter"},
		{"the same counter again", func() error {
			c, d, s := a.assertion(rp.ID, origin, chal, flagUP|flagUV, 5)
			return verify(chal, reg.PublicKey, 5, c, d, s)
		}, "counter"},
		{"garbage", func() error { return verify(chal, reg.PublicKey, 0, []byte("x"), []byte("y"), []byte("z")) }, ""},
		{"a broken key", func() error { return verify(chal, []byte{0xa1}, 0, cd, ad, sig) }, ""},
	} {
		if err := c.run(); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	// the counter that goes up is fine
	c, d, s := a.assertion(rp.ID, origin, chal, flagUP|flagUV, 10)
	if n, err := rp.VerifyAssertion(chal, reg.PublicKey, 9, c, d, s); err != nil || n != 10 {
		t.Errorf("%d %v", n, err)
	}
}

func TestRegistrationIsRefusedWhenAnythingIsWrong(t *testing.T) {
	a := newAuthenticator(t, "es256")
	chal := NewChallenge()
	cd := clientJSON("webauthn.create", chal, origin)
	ad := buildAuthData(rp.ID, flagUP|flagUV|flagAT, 0, a.attested())
	for name, run := range map[string]func() error{
		"another challenge": func() error { _, e := rp.ParseRegistration(NewChallenge(), cd, attObj(ad)); return e },
		"a login answer": func() error {
			_, e := rp.ParseRegistration(chal, clientJSON("webauthn.get", chal, origin), attObj(ad))
			return e
		},
		"another origin": func() error {
			_, e := rp.ParseRegistration(chal, clientJSON("webauthn.create", chal, "http://pdu.example.com"), attObj(ad))
			return e
		},
		"another site": func() error {
			_, e := rp.ParseRegistration(chal, cd, attObj(buildAuthData("evil.example.com", flagUP|flagUV|flagAT, 0, a.attested())))
			return e
		},
		"without verification": func() error {
			_, e := rp.ParseRegistration(chal, cd, attObj(buildAuthData(rp.ID, flagUP|flagAT, 0, a.attested())))
			return e
		},
		"without a credential": func() error {
			_, e := rp.ParseRegistration(chal, cd, attObj(buildAuthData(rp.ID, flagUP|flagUV, 0, nil)))
			return e
		},
		"a point that is not on the curve": func() error {
			bad := cMap(cInt(1), cInt(2), cInt(3), cInt(-7), cInt(-1), cInt(1), cInt(-2), cBytes(pad32([]byte{1})), cInt(-3), cBytes(pad32([]byte{2})))
			x := make([]byte, 16)
			x = binary.BigEndian.AppendUint16(x, 4)
			x = append(append(x, "abcd"...), bad...)
			_, e := rp.ParseRegistration(chal, cd, attObj(buildAuthData(rp.ID, flagUP|flagUV|flagAT, 0, x)))
			return e
		},
		"a key of a kind that is not supported": func() error {
			bad := cMap(cInt(1), cInt(2), cInt(3), cInt(-35))
			x := make([]byte, 16)
			x = binary.BigEndian.AppendUint16(x, 4)
			x = append(append(x, "abcd"...), bad...)
			_, e := rp.ParseRegistration(chal, cd, attObj(buildAuthData(rp.ID, flagUP|flagUV|flagAT, 0, x)))
			return e
		},
		"a small RSA key": func() error {
			k, _ := rsa.GenerateKey(rand.Reader, 1024)
			bad := cMap(cInt(1), cInt(3), cInt(3), cInt(-257), cInt(-1), cBytes(k.N.Bytes()), cInt(-2), cBytes([]byte{1, 0, 1}))
			x := make([]byte, 16)
			x = binary.BigEndian.AppendUint16(x, 4)
			x = append(append(x, "abcd"...), bad...)
			_, e := rp.ParseRegistration(chal, cd, attObj(buildAuthData(rp.ID, flagUP|flagUV|flagAT, 0, x)))
			return e
		},
		"not CBOR":     func() error { _, e := rp.ParseRegistration(chal, cd, []byte("not cbor")); return e },
		"not a map":    func() error { _, e := rp.ParseRegistration(chal, cd, cText("x")); return e },
		"no auth data": func() error { _, e := rp.ParseRegistration(chal, cd, cMap(cText("fmt"), cText("none"))); return e },
	} {
		if err := run(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := rp.ParseRegistration(chal, cd, attObj(ad)); err != nil {
		t.Fatal(err)
	}
}

func TestCBORDecoderLimits(t *testing.T) {
	for name, b := range map[string][]byte{
		"empty":                         {},
		"indefinite array":              {0x9f, 0x01, 0xff},
		"a string longer than the data": {0x58, 0xff, 'a'},
		"a huge array":                  {0x9b, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff},
		"a float":                       {0xfb, 0, 0, 0, 0, 0, 0, 0, 0},
		"a tag":                         {0xc1, 0x01},
		"a map with a byte key":         {0xa1, 0x41, 0x01, 0x01},
		"the same key twice":            {0xa2, 0x01, 0x01, 0x01, 0x02},
		"a cut argument":                {0x19, 0x01},
	} {
		if _, _, err := decodeCBOR(b); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	deep := make([]byte, 0)
	for i := 0; i < 20; i++ {
		deep = append(deep, 0x81)
	}
	if _, _, err := decodeCBOR(append(deep, 0x01)); err == nil {
		t.Error("too deep")
	}
	if v, n, err := decodeCBOR([]byte{0x82, 0x01, 0x20, 0xff}); err != nil || n != 3 || len(v.([]any)) != 2 || v.([]any)[1] != int64(-1) {
		t.Errorf("%v %d %v", v, n, err)
	}
}
