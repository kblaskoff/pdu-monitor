package totp

import (
	"strings"
	"testing"
	"time"
)

// RFC 6238, appendix B: the secret is "12345678901234567890", SHA-1; the RFC lists 8 digits, these are the last 6 of them.
func TestRFC6238Vectors(t *testing.T) {
	secret := []byte("12345678901234567890")
	for unix, want := range map[int64]string{59: "287082", 1111111109: "081804", 1111111111: "050471", 1234567890: "005924", 2000000000: "279037", 20000000000: "353130"} {
		if got := Code(secret, Step(time.Unix(unix, 0))); got != want {
			t.Errorf("t=%d: %s, want %s", unix, got, want)
		}
	}
}

func TestVerifyWindowAndReplay(t *testing.T) {
	secret := NewSecret()
	now := time.Unix(1_800_000_000, 0)
	cur := Step(now)
	if step, ok := Verify(secret, Code(secret, cur), now, 0); !ok || step != cur {
		t.Fatal("the current code")
	}
	if _, ok := Verify(secret, Code(secret, cur-1), now, 0); !ok {
		t.Error("the code of the period before (a slow clock)")
	}
	if _, ok := Verify(secret, Code(secret, cur+1), now, 0); !ok {
		t.Error("the code of the period after (a fast clock)")
	}
	if _, ok := Verify(secret, Code(secret, cur-2), now, 0); ok {
		t.Error("two periods old")
	}
	if _, ok := Verify(secret, Code(secret, cur+2), now, 0); ok {
		t.Error("two periods ahead")
	}
	// a code that was accepted cannot be used again, nor any older one
	if _, ok := Verify(secret, Code(secret, cur), now, cur); ok {
		t.Error("the same period again")
	}
	if _, ok := Verify(secret, Code(secret, cur-1), now, cur); ok {
		t.Error("an older period after a newer one was used")
	}
	if _, ok := Verify(secret, Code(secret, cur+1), now, cur); !ok {
		t.Error("the next period is fine")
	}
	for _, bad := range []string{"", "12345", "1234567", "abcdef", "000000 "} {
		if code := Code(secret, cur); bad != code {
			if _, ok := Verify(secret, bad, now, 0); ok && bad != "" {
				t.Errorf("%q accepted", bad)
			}
		}
	}
	// a person types the code with a space in the middle, as the apps show it
	c := Code(secret, cur)
	if _, ok := Verify(secret, c[:3]+" "+c[3:], now, 0); !ok {
		t.Error("a code with a space")
	}
	// another secret
	if _, ok := Verify(NewSecret(), Code(secret, cur), now, 0); ok {
		t.Error("another secret")
	}
}

func TestURIHasWhatTheAppsRead(t *testing.T) {
	u := URI("PDU Monitor", "anna@example", []byte("12345678901234567890"))
	for _, want := range []string{"otpauth://totp/PDU%20Monitor:anna@example?", "secret=GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ", "issuer=PDU+Monitor", "digits=6", "period=30", "algorithm=SHA1"} {
		if !strings.Contains(u, want) {
			t.Errorf("%s misses %s", u, want)
		}
	}
	if len(NewSecret()) != SecretSize || EncodeSecret(NewSecret()) == EncodeSecret(NewSecret()) {
		t.Error("secrets")
	}
}
