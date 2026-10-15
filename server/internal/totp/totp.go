// Package totp is the one-time code of an authenticator app (RFC 6238: HMAC-SHA1, 30 seconds, 6 digits: what Google Authenticator,
// Microsoft Authenticator, Authy, 1Password and the like all use).
package totp

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"net/url"
	"strings"
	"time"
)

const (
	Period     = 30 * time.Second
	Digits     = 6
	SecretSize = 20 // bytes: 160 bits, what RFC 4226 recommends
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewSecret returns a random secret.
func NewSecret() []byte {
	b := make([]byte, SecretSize)
	if _, err := rand.Read(b); err != nil {
		panic(err) // no randomness: nothing safe can be done
	}
	return b
}

// EncodeSecret is the text the person types into an app (without padding, which is what apps expect).
func EncodeSecret(secret []byte) string { return b32.EncodeToString(secret) }

// Step is the number of the 30 second period that t is in.
func Step(t time.Time) int64 { return t.Unix() / int64(Period.Seconds()) }

// Code is the code of one period (RFC 4226 dynamic truncation).
func Code(secret []byte, step int64) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(step))
	mac := hmac.New(sha1.New, secret)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	n := (uint32(sum[off]&0x7f) << 24) | uint32(sum[off+1])<<16 | uint32(sum[off+2])<<8 | uint32(sum[off+3])
	var mod uint32 = 1
	for i := 0; i < Digits; i++ {
		mod *= 10
	}
	s := make([]byte, Digits)
	n %= mod
	for i := Digits - 1; i >= 0; i-- {
		s[i] = byte('0' + n%10)
		n /= 10
	}
	return string(s)
}

// Verify checks a code at time `now`, accepting the period before and after the current one (clocks differ by a little). lastStep is the
// newest period that was accepted for this secret before: a code of that period or an older one is refused, so that a code that was
// seen once (over a shoulder, in a log) cannot be used again. It returns the period that matched.
func Verify(secret []byte, code string, now time.Time, lastStep int64) (step int64, ok bool) {
	code = strings.ReplaceAll(strings.TrimSpace(code), " ", "")
	if len(code) != Digits {
		return 0, false
	}
	cur := Step(now)
	found := int64(-1)
	for _, s := range []int64{cur - 1, cur, cur + 1} { // all are compared, so that the time does not tell which one matched
		if subtle.ConstantTimeCompare([]byte(code), []byte(Code(secret, s))) == 1 && s > lastStep {
			found = s
		}
	}
	if found < 0 {
		return 0, false
	}
	return found, true
}

// URI is what the QR code holds (the Key URI Format that the apps read).
func URI(issuer, account string, secret []byte) string {
	label := url.PathEscape(issuer) + ":" + url.PathEscape(account)
	q := url.Values{}
	q.Set("secret", EncodeSecret(secret))
	q.Set("issuer", issuer)
	q.Set("algorithm", "SHA1")
	q.Set("digits", "6")
	q.Set("period", "30")
	return "otpauth://totp/" + label + "?" + q.Encode()
}
