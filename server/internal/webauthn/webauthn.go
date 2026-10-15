// Package webauthn checks passkeys (WebAuthn level 2: a fingerprint on a laptop, a phone, a security key) as a second step of the login. It
// does the two things that are needed and nothing more: take in a new credential (registration) and check a signature of an existing one
// (assertion). The attestation of the authenticator is not checked (the server asks for none): what counts is that the same key signs
// later, for this site, with the person present and verified (fingerprint or PIN). ES256, RS256 and EdDSA keys are understood: what
// Touch ID, Windows Hello, Android and the security keys make.
package webauthn

import (
	"crypto"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
)

// RP is this site as the authenticators know it: the id is a host name (not an address), and the origin is what the browser shows.
type RP struct {
	ID      string
	Origins []string
}

const (
	flagUP = 0x01 // the person was present
	flagUV = 0x04 // the person was verified (fingerprint, PIN)
	flagAT = 0x40 // attested credential data follows

	maxCredentialID = 1023
)

// NewChallenge is 32 random bytes.
func NewChallenge() []byte {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}

// B64 is the encoding the browser APIs and this package use for binary values (base64url without padding).
func B64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// Decode is the reverse of B64.
func Decode(s string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(s) }

// Registration is a new credential.
type Registration struct {
	CredentialID []byte
	PublicKey    []byte // the COSE key as the authenticator made it; kept as it is and read again when a signature is checked
	SignCount    uint32
}

type clientData struct {
	Type        string `json:"type"`
	Challenge   string `json:"challenge"`
	Origin      string `json:"origin"`
	CrossOrigin *bool  `json:"crossOrigin"`
}

func (rp RP) checkClientData(raw []byte, wantType string, challenge []byte) error {
	var cd clientData
	if err := json.Unmarshal(raw, &cd); err != nil {
		return errors.New("the answer of the browser is not valid")
	}
	if cd.Type != wantType {
		return errors.New("the answer is for something else")
	}
	if subtle.ConstantTimeCompare([]byte(cd.Challenge), []byte(B64(challenge))) != 1 || len(challenge) == 0 {
		return errors.New("the answer is not for this login (the challenge differs)")
	}
	if cd.CrossOrigin != nil && *cd.CrossOrigin {
		return errors.New("the answer came from another site")
	}
	for _, o := range rp.Origins {
		if cd.Origin == o {
			return nil
		}
	}
	return fmt.Errorf("the answer is for another address (%s)", cd.Origin)
}

type authData struct {
	flags     byte
	count     uint32
	attested  bool
	credID    []byte
	publicKey []byte
}

func (rp RP) parseAuthData(b []byte, needAttested bool) (*authData, error) {
	if len(b) < 37 {
		return nil, errors.New("the authenticator data is too short")
	}
	want := sha256.Sum256([]byte(rp.ID))
	if subtle.ConstantTimeCompare(b[:32], want[:]) != 1 {
		return nil, errors.New("the answer is for another site (the relying party differs)")
	}
	a := &authData{flags: b[32], count: binary.BigEndian.Uint32(b[33:37])}
	if a.flags&flagUP == 0 {
		return nil, errors.New("the person was not present")
	}
	if a.flags&flagUV == 0 {
		return nil, errors.New("the person was not verified (a fingerprint or a PIN is needed)")
	}
	if a.flags&flagAT != 0 {
		rest := b[37:]
		if len(rest) < 18 {
			return nil, errors.New("the credential data is too short")
		}
		n := int(binary.BigEndian.Uint16(rest[16:18]))
		if n == 0 || n > maxCredentialID || len(rest) < 18+n {
			return nil, errors.New("the credential id is not valid")
		}
		a.attested = true
		a.credID = append([]byte(nil), rest[18:18+n]...)
		key := rest[18+n:]
		_, used, err := decodeCBOR(key)
		if err != nil {
			return nil, err
		}
		a.publicKey = append([]byte(nil), key[:used]...)
	}
	if needAttested && !a.attested {
		return nil, errors.New("there is no credential in the answer")
	}
	return a, nil
}

// ParseRegistration checks the answer of navigator.credentials.create() and returns the new credential.
func (rp RP) ParseRegistration(challenge, clientDataJSON, attestationObject []byte) (*Registration, error) {
	if err := rp.checkClientData(clientDataJSON, "webauthn.create", challenge); err != nil {
		return nil, err
	}
	v, _, err := decodeCBOR(attestationObject)
	if err != nil {
		return nil, err
	}
	obj, ok := v.(map[any]any)
	if !ok {
		return nil, errCBOR
	}
	raw, ok := obj["authData"].([]byte)
	if !ok {
		return nil, errors.New("the answer has no authenticator data")
	}
	a, err := rp.parseAuthData(raw, true)
	if err != nil {
		return nil, err
	}
	if _, err := parseKey(a.publicKey); err != nil { // refused now, not when it is used for the first time
		return nil, err
	}
	return &Registration{CredentialID: a.credID, PublicKey: a.publicKey, SignCount: a.count}, nil
}

// VerifyAssertion checks the answer of navigator.credentials.get() against a stored credential and returns the new signature counter.
// Many passkeys (synced ones) always count 0: that is accepted; a counter that goes back, when it is used at all, is not.
func (rp RP) VerifyAssertion(challenge, publicKey []byte, storedCount uint32, clientDataJSON, authenticatorData, signature []byte) (uint32, error) {
	if err := rp.checkClientData(clientDataJSON, "webauthn.get", challenge); err != nil {
		return 0, err
	}
	a, err := rp.parseAuthData(authenticatorData, false)
	if err != nil {
		return 0, err
	}
	key, err := parseKey(publicKey)
	if err != nil {
		return 0, err
	}
	hash := sha256.Sum256(clientDataJSON)
	msg := append(append([]byte(nil), authenticatorData...), hash[:]...)
	if !key.verify(msg, signature) {
		return 0, errors.New("the signature is not valid")
	}
	if (a.count != 0 || storedCount != 0) && a.count <= storedCount {
		return 0, errors.New("the counter of the key went back: the key may have been copied")
	}
	return a.count, nil
}

// ---- keys

type coseKey struct {
	ec  *ecdsa.PublicKey
	rsa *rsa.PublicKey
	ed  ed25519.PublicKey
}

func (k *coseKey) verify(msg, sig []byte) bool {
	switch {
	case k.ec != nil:
		h := sha256.Sum256(msg)
		return ecdsa.VerifyASN1(k.ec, h[:], sig)
	case k.rsa != nil:
		h := sha256.Sum256(msg)
		return rsa.VerifyPKCS1v15(k.rsa, crypto.SHA256, h[:], sig) == nil
	case k.ed != nil:
		return len(sig) == ed25519.SignatureSize && ed25519.Verify(k.ed, msg, sig)
	}
	return false
}

func intOf(m map[any]any, k int64) (int64, bool) { v, ok := m[k].(int64); return v, ok }
func bytesOf(m map[any]any, k int64) ([]byte, bool) {
	v, ok := m[k].([]byte)
	return v, ok
}

func parseKey(raw []byte) (*coseKey, error) {
	v, n, err := decodeCBOR(raw)
	if err != nil || n != len(raw) {
		return nil, errors.New("the public key is not valid")
	}
	m, ok := v.(map[any]any)
	if !ok {
		return nil, errors.New("the public key is not valid")
	}
	kty, _ := intOf(m, 1)
	alg, _ := intOf(m, 3)
	switch {
	case kty == 2 && alg == -7: // EC2, ES256
		crv, _ := intOf(m, -1)
		x, okx := bytesOf(m, -2)
		y, oky := bytesOf(m, -3)
		if crv != 1 || !okx || !oky || len(x) != 32 || len(y) != 32 {
			return nil, errors.New("the public key is not an ES256 key")
		}
		point := append([]byte{4}, append(append([]byte(nil), x...), y...)...)
		if _, err := ecdh.P256().NewPublicKey(point); err != nil { // the point must be on the curve
			return nil, errors.New("the public key is not valid")
		}
		return &coseKey{ec: &ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}}, nil
	case kty == 3 && alg == -257: // RSA, RS256
		nb, okn := bytesOf(m, -1)
		eb, oke := bytesOf(m, -2)
		if !okn || !oke || len(nb) < 256 || len(nb) > 1024 || len(eb) == 0 || len(eb) > 4 { // at least 2048 bits
			return nil, errors.New("the public key is not an RS256 key of a usable size")
		}
		e := 0
		for _, c := range eb {
			e = e<<8 | int(c)
		}
		if e < 3 || e%2 == 0 {
			return nil, errors.New("the public key is not valid")
		}
		return &coseKey{rsa: &rsa.PublicKey{N: new(big.Int).SetBytes(nb), E: e}}, nil
	case kty == 1 && alg == -8: // OKP, EdDSA
		crv, _ := intOf(m, -1)
		x, okx := bytesOf(m, -2)
		if crv != 6 || !okx || len(x) != ed25519.PublicKeySize {
			return nil, errors.New("the public key is not an EdDSA key")
		}
		return &coseKey{ed: ed25519.PublicKey(append([]byte(nil), x...))}, nil
	}
	return nil, errors.New("this kind of key is not supported")
}
