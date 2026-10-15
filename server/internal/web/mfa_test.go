package web_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/binary"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kblaskoff/pdu-monitor/server/internal/totp"
	"github.com/kblaskoff/pdu-monitor/server/internal/webauthn"
)

// ---- helpers

func secretOf(t *testing.T, text string) []byte {
	t.Helper()
	b, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(text)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// enrollTOTP adds an authenticator app for the logged in person and returns its secret.
func enrollTOTP(t *testing.T, c *client, name string) ([]byte, map[string]any) {
	t.Helper()
	status, out := c.call("POST", "/api/me/mfa/totp/begin", map[string]string{"name": name})
	if status != 200 || out["secret"] == nil || len(list(out["qr"])) < 20 || !strings.HasPrefix(out["uri"].(string), "otpauth://totp/") {
		t.Fatalf("begin: %d %v", status, out)
	}
	secret := secretOf(t, out["secret"].(string))
	status, done := c.call("POST", "/api/me/mfa/totp/confirm", map[string]string{"code": totp.Code(secret, totp.Step(time.Now()))})
	if status != 200 {
		t.Fatalf("confirm: %d %v", status, done)
	}
	return secret, done
}

// passwordStep does the first step as `user` and returns what the server answered.
func passwordStep(e *env, c *client, user string) (int, map[string]any) {
	return c.call("POST", "/api/login", map[string]string{"username": user, "password": "pw"}, true)
}

func codeIn(secret []byte, shift int64) string { return totp.Code(secret, totp.Step(time.Now())+shift) }

// ---- authenticator apps

func TestAuthenticatorAppSecondStep(t *testing.T) {
	e := newEnv(t)
	anna := e.login("anna")
	// nothing to confirm with a wrong code, and nothing is kept
	if status, _ := anna.call("POST", "/api/me/mfa/totp/begin", map[string]string{"name": "phone"}); status != 200 {
		t.Fatal(status)
	}
	if status, _ := anna.call("POST", "/api/me/mfa/totp/confirm", map[string]string{"code": "000000"}); status != 401 {
		t.Errorf("a wrong code at the setting up: %d", status)
	}
	if _, out := anna.call("GET", "/api/me/mfa", nil); len(list(out["factors"])) != 0 {
		t.Fatalf("%v", out)
	}
	secret, done := enrollTOTP(t, anna, "phone")
	codes := list(done["recoveryCodes"])
	if len(codes) != 10 || len(codes[0].(string)) != 11 || done["recovery"].(float64) != 10 {
		t.Fatalf("the first device brings the recovery codes: %v", done)
	}

	// a new login: the password is not enough
	c := e.anonymous()
	status, out := passwordStep(e, c, "anna")
	token, _ := out["token"].(string)
	if status != 200 || out["mfa"] != true || token == "" || out["csrf"] != nil || obj(out["methods"])["totp"] != true || obj(out["methods"])["passkey"] != false || obj(out["methods"])["recovery"] != true {
		t.Fatalf("%d %v", status, out)
	}
	if status, _ := c.call("GET", "/api/overview", nil); status != 401 {
		t.Errorf("a session after the password only: %d", status)
	}
	if status, _ := c.call("POST", "/api/login/mfa", map[string]any{"token": "nope", "method": "totp", "code": codeIn(secret, 0)}, true); status != 401 {
		t.Errorf("an unknown token: %d", status)
	}
	if status, _ := c.call("POST", "/api/login/mfa", map[string]any{"token": token, "method": "totp", "code": "123456"}, true); status != 401 {
		t.Errorf("a wrong code: %d", status)
	}
	if status, _ = c.call("GET", "/api/overview", nil); status != 401 {
		t.Error("still no session")
	}
	// the code of the moment when the app was set up was used: it does not open a login (the next one does)
	if status, _ := c.call("POST", "/api/login/mfa", map[string]any{"token": token, "method": "totp", "code": codeIn(secret, 0)}, true); status != 401 {
		t.Errorf("the code that confirmed the setting up opened a login: %d", status)
	}
	status, out = c.call("POST", "/api/login/mfa", map[string]any{"token": token, "method": "totp", "code": codeIn(secret, 1)}, true)
	if status != 200 || out["csrf"] == nil || out["mfa"] != true {
		t.Fatalf("%d %v", status, out)
	}
	c.csrf = out["csrf"].(string)
	if status, _ := c.call("GET", "/api/overview", nil); status != 200 {
		t.Errorf("logged in: %d", status)
	}
	if status, _ := c.call("POST", "/api/login/mfa", map[string]any{"token": token, "method": "totp", "code": codeIn(secret, 1)}, true); status != 401 {
		t.Errorf("the token of a finished login works again: %d", status)
	}
	// a code that was used cannot be used again, by a second login
	c2 := e.anonymous()
	_, out = passwordStep(e, c2, "anna")
	if status, _ := c2.call("POST", "/api/login/mfa", map[string]any{"token": out["token"], "method": "totp", "code": codeIn(secret, 1)}, true); status != 401 {
		t.Errorf("a code twice: %d", status)
	}
	// another person's password does not give their second step to somebody else
	c3 := e.anonymous()
	_, out = passwordStep(e, c3, "boris")
	if out["mfa"] == true || out["csrf"] == nil {
		t.Errorf("a person without a second step logs in as before: %v", out)
	}
}

func TestWrongCodesLockThePasswordCheck(t *testing.T) {
	e := newEnv(t)
	anna := e.login("anna")
	secret, _ := enrollTOTP(t, anna, "phone")
	c := e.anonymous()
	_, out := passwordStep(e, c, "anna")
	token := out["token"]
	for i := 0; i < 5; i++ {
		if status, _ := c.call("POST", "/api/login/mfa", map[string]any{"token": token, "method": "totp", "code": "111111"}, true); status != 401 {
			t.Fatalf("%d: %d", i, status)
		}
	}
	// after five wrong codes the password check is void, also for the right code
	status, out := c.call("POST", "/api/login/mfa", map[string]any{"token": token, "method": "totp", "code": codeIn(secret, 1)}, true)
	if status != 401 || out["expired"] != true {
		t.Errorf("%d %v", status, out)
	}
	// and the person is locked for a while (8 wrong codes in 15 minutes, over all the logins): even the right code is refused
	_, o := passwordStep(e, c, "anna")
	for i := 0; i < 3; i++ {
		c.call("POST", "/api/login/mfa", map[string]any{"token": o["token"], "method": "totp", "code": "111111"}, true)
	}
	_, o = passwordStep(e, c, "anna")
	if status, _ := c.call("POST", "/api/login/mfa", map[string]any{"token": o["token"], "method": "totp", "code": codeIn(secret, 1)}, true); status != 429 {
		t.Errorf("the right code after many wrong ones: %d", status)
	}
}

func TestRecoveryCodeWorksOnce(t *testing.T) {
	e := newEnv(t)
	anna := e.login("anna")
	_, done := enrollTOTP(t, anna, "phone")
	code := list(done["recoveryCodes"])[3].(string)
	login := func(code string) int {
		c := e.anonymous()
		_, out := passwordStep(e, c, "anna")
		status, _ := c.call("POST", "/api/login/mfa", map[string]any{"token": out["token"], "method": "recovery", "code": code}, true)
		return status
	}
	if login(strings.ToLower(strings.ReplaceAll(code, "-", " "))) != 200 { // typed in lower case, with a space
		t.Error("a recovery code")
	}
	if login(code) != 401 {
		t.Error("the same recovery code twice")
	}
	if login("AAAAA-AAAAA") != 401 {
		t.Error("a made up code")
	}
	_, out := anna.call("GET", "/api/me/mfa", nil)
	if out["recovery"].(float64) != 9 {
		t.Errorf("%v", out["recovery"])
	}
	// new codes: the old ones stop working
	status, out := anna.call("POST", "/api/me/mfa/recovery", map[string]any{})
	if status != 200 || len(list(out["recoveryCodes"])) != 10 {
		t.Fatalf("%d %v", status, out)
	}
	if login(list(done["recoveryCodes"])[0].(string)) != 401 {
		t.Error("an old code after new ones were made")
	}
	if login(list(out["recoveryCodes"])[0].(string)) != 200 {
		t.Error("a new code")
	}
}

func TestAtMostThreeAuthenticatorApps(t *testing.T) {
	e := newEnv(t)
	anna := e.login("anna")
	for i := 0; i < 3; i++ {
		enrollTOTP(t, anna, "phone "+string(rune('1'+i)))
	}
	if status, _ := anna.call("POST", "/api/me/mfa/totp/begin", map[string]string{"name": "a fourth"}); status != 409 {
		t.Errorf("a fourth: %d", status)
	}
	if status, _ := anna.call("POST", "/api/me/mfa/totp/begin", map[string]string{"name": ""}); status != 400 {
		t.Errorf("no name: %d", status)
	}
	_, out := anna.call("GET", "/api/me/mfa", nil)
	fs := list(out["factors"])
	if len(fs) != 3 || obj(fs[0])["name"] != "phone 1" {
		t.Fatalf("%v", out)
	}
	// each device has its own secret, so one can be removed alone
	if status, _ := anna.call("DELETE", "/api/me/mfa/"+itoa(int(obj(fs[0])["id"].(float64))), nil); status != 200 {
		t.Error("remove")
	}
	if status, _ := anna.call("POST", "/api/me/mfa/totp/begin", map[string]string{"name": "a new one"}); status != 200 {
		t.Error("room again")
	}
	// the devices of one person are not reachable by another
	boris := e.login("boris")
	if status, _ := boris.call("DELETE", "/api/me/mfa/"+itoa(int(obj(fs[1])["id"].(float64))), nil); status != 404 {
		t.Errorf("another person removed it: %d", status)
	}
}

// ---- who may do what

func TestRequiredSecondStepLimitsTheSessionUntilItIsSetUp(t *testing.T) {
	e := newEnv(t)
	anna := e.login("anna")
	if status, _ := anna.call("PUT", "/api/admin/settings", map[string]any{"requireMfa": true}); status != 200 {
		t.Fatal(status)
	}
	boris := e.anonymous()
	status, out := passwordStep(e, boris, "boris")
	if status != 200 || out["limited"] != true {
		t.Fatalf("%d %v", status, out)
	}
	boris.csrf = out["csrf"].(string)
	status, out = boris.call("GET", "/api/overview", nil)
	if status != 403 || out["mfaSetup"] != true {
		t.Errorf("a limited session sees data: %d %v", status, out)
	}
	if status, _ := boris.call("GET", "/api/admin/config", nil); status != 403 {
		t.Errorf("%d", status)
	}
	if status, _ := boris.call("GET", "/api/me/mfa", nil); status != 200 {
		t.Errorf("it must be able to set up: %d", status)
	}
	// setting up the first device makes the session a full one
	enrollTOTP(t, boris, "phone")
	if status, _ := boris.call("GET", "/api/overview", nil); status != 200 {
		t.Errorf("after setting up: %d", status)
	}
	// and the last device cannot be removed while it is required
	_, mine := boris.call("GET", "/api/me/mfa", nil)
	id := int(obj(list(mine["factors"])[0])["id"].(float64))
	if status, _ := boris.call("DELETE", "/api/me/mfa/"+itoa(id), nil); status != 409 {
		t.Errorf("the last device removed while it is required: %d", status)
	}
	// logging out is always possible
	limited := e.anonymous()
	_, o := passwordStep(e, limited, "vera")
	limited.csrf = o["csrf"].(string)
	if status, _ := limited.call("POST", "/api/logout", map[string]any{}); status != 200 {
		t.Errorf("a limited session must be able to log out: %d", status)
	}
}

func TestAdministratorResetsSomebodyWhoLostEverything(t *testing.T) {
	e := newEnv(t)
	boris := e.login("boris")
	enrollTOTP(t, boris, "phone")
	anna := e.login("anna")
	_, out := anna.call("GET", "/api/admin/mfa", nil)
	users := list(out["users"])
	if len(users) != 1 || obj(users[0])["user"] != "ldap:boris" || obj(users[0])["totp"].(float64) != 1 {
		t.Fatalf("%v", out)
	}
	vera := e.login("vera")
	if status, _ := vera.call("GET", "/api/admin/mfa", nil); status != 403 {
		t.Errorf("%d", status)
	}
	if status, _ := vera.call("DELETE", "/api/admin/mfa?user=ldap:boris", nil); status != 403 {
		t.Errorf("a viewer reset it: %d", status)
	}
	// boris has a session; the reset logs him out everywhere
	if status, _ := boris.call("GET", "/api/overview", nil); status != 200 {
		t.Fatal(status)
	}
	if status, out := anna.call("DELETE", "/api/admin/mfa?user=ldap:boris", nil); status != 200 || out["removed"].(float64) != 1 {
		t.Fatalf("%d %v", status, out)
	}
	if status, _ := boris.call("GET", "/api/overview", nil); status != 401 {
		t.Errorf("the sessions of the person remain: %d", status)
	}
	again := e.anonymous()
	if _, o := passwordStep(e, again, "boris"); o["mfa"] == true {
		t.Error("the second step remains")
	}
}

func TestHowLongPeopleStayLoggedIn(t *testing.T) {
	e := newEnv(t)
	anna := e.login("anna")
	if status, _ := anna.call("PUT", "/api/admin/settings", map[string]any{"sessionHours": 72}); status != 200 {
		t.Fatal(status)
	}
	c := e.anonymous()
	resp, _ := c.raw("POST", "/api/login", map[string]string{"username": "vera", "password": "pw"}, map[string]string{})
	var maxAge int
	for _, ck := range resp.Cookies() {
		if ck.Name == "pdum_session" {
			maxAge = ck.MaxAge
		}
	}
	if maxAge != 72*3600 {
		t.Errorf("the cookie lives %d s, want three days", maxAge)
	}
	_, s := anna.call("GET", "/api/admin/settings", nil)
	if s["sessionHours"].(float64) != 72 || s["requireMfa"] != false {
		t.Errorf("%v", s)
	}
	if status, _ := anna.call("PUT", "/api/admin/settings", map[string]any{"sessionHours": 100000}); status != 200 {
		t.Fatal(status)
	}
	_, s = anna.call("GET", "/api/admin/settings", nil)
	if s["sessionHours"].(float64) != 72 {
		t.Errorf("an absurd value is replaced by the default: %v", s["sessionHours"])
	}
}

// ---- passkeys, with a software authenticator

type softKey struct {
	t    *testing.T
	key  *ecdsa.PrivateKey
	id   []byte
	rpID string
	n    uint32
}

func b64(b []byte) string { return webauthn.B64(b) }

func cbHead(major byte, n int) []byte {
	if n < 24 {
		return []byte{major<<5 | byte(n)}
	}
	if n < 256 {
		return []byte{major<<5 | 24, byte(n)}
	}
	return []byte{major<<5 | 25, byte(n >> 8), byte(n)}
}
func cbInt(n int64) []byte {
	if n >= 0 {
		return cbHead(0, int(n))
	}
	return cbHead(1, int(-1-n))
}
func cbBytes(b []byte) []byte { return append(cbHead(2, len(b)), b...) }
func cbText(s string) []byte  { return append(cbHead(3, len(s)), s...) }
func cbMap(parts ...[]byte) []byte {
	out := cbHead(5, len(parts)/2)
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func newSoftKey(t *testing.T, rpID string) *softKey {
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	id := make([]byte, 24)
	rand.Read(id)
	return &softKey{t: t, key: k, id: id, rpID: rpID}
}

func pad(b []byte) []byte { return append(make([]byte, 32-len(b)), b...) }

func (k *softKey) authData(flags byte, attested bool) []byte {
	h := sha256.Sum256([]byte(k.rpID))
	out := append(h[:], flags)
	out = binary.BigEndian.AppendUint32(out, k.n)
	if attested {
		out = append(out, make([]byte, 16)...)
		out = binary.BigEndian.AppendUint16(out, uint16(len(k.id)))
		out = append(out, k.id...)
		out = append(out, cbMap(cbInt(1), cbInt(2), cbInt(3), cbInt(-7), cbInt(-1), cbInt(1), cbInt(-2), cbBytes(pad(k.key.X.Bytes())), cbInt(-3), cbBytes(pad(k.key.Y.Bytes())))...)
	}
	return out
}

func clientData(typ, challenge, origin string) []byte {
	b, _ := json.Marshal(map[string]any{"type": typ, "challenge": challenge, "origin": origin})
	return b
}

// register answers the options that the server gave, like navigator.credentials.create() does
func (k *softKey) register(options map[string]any, origin string) map[string]any {
	ad := k.authData(0x01|0x04|0x40, true)
	return map[string]any{"id": b64(k.id), "clientDataJSON": b64(clientData("webauthn.create", options["challenge"].(string), origin)),
		"attestationObject": b64(cbMap(cbText("fmt"), cbText("none"), cbText("attStmt"), cbMap(), cbText("authData"), cbBytes(ad)))}
}

// assert answers a question, like navigator.credentials.get() does
func (k *softKey) assert(options map[string]any, origin string, flags byte) map[string]any {
	k.n++
	cd := clientData("webauthn.get", options["challenge"].(string), origin)
	ad := k.authData(flags, false)
	h := sha256.Sum256(cd)
	sum := sha256.Sum256(append(append([]byte(nil), ad...), h[:]...))
	sig, _ := ecdsa.SignASN1(rand.Reader, k.key, sum[:])
	return map[string]any{"id": b64(k.id), "clientDataJSON": b64(cd), "authenticatorData": b64(ad), "signature": b64(sig)}
}

const rpOrigin = "https://pdu.example.com"

func withPasskeys(e *env) {
	e.srv.RP = &webauthn.RP{ID: "pdu.example.com", Origins: []string{rpOrigin}}
}

func TestPasskeySecondStep(t *testing.T) {
	e := newEnv(t)
	anna := e.login("anna")
	// without public_url passkeys are refused with a reason
	if status, out := anna.call("POST", "/api/me/mfa/passkey/begin", map[string]string{"name": "laptop"}); status != 409 || !strings.Contains(out["error"].(string), "public_url") {
		t.Fatalf("%d %v", status, out)
	}
	withPasskeys(e)
	key := newSoftKey(t, "pdu.example.com")
	status, opts := anna.call("POST", "/api/me/mfa/passkey/begin", map[string]string{"name": "laptop"})
	if status != 200 || obj(opts["rp"])["id"] != "pdu.example.com" || obj(opts["authenticatorSelection"])["userVerification"] != "required" || opts["attestation"] != "none" {
		t.Fatalf("%d %v", status, opts)
	}
	// a registration for another site is refused
	wrong := newSoftKey(t, "evil.example.com")
	if status, _ := anna.call("POST", "/api/me/mfa/passkey/finish", wrong.register(opts, rpOrigin)); status != 400 {
		t.Errorf("a key of another site: %d", status)
	}
	// (the open question is gone after one answer: ask again)
	_, opts = anna.call("POST", "/api/me/mfa/passkey/begin", map[string]string{"name": "laptop"})
	status, done := anna.call("POST", "/api/me/mfa/passkey/finish", key.register(opts, rpOrigin))
	if status != 200 || len(list(done["recoveryCodes"])) != 10 || len(list(done["factors"])) != 1 {
		t.Fatalf("%d %v", status, done)
	}
	if status, _ := anna.call("POST", "/api/me/mfa/passkey/finish", key.register(opts, rpOrigin)); status != 409 {
		t.Errorf("the same answer twice: %d", status)
	}

	// login: the password, then the question, then the signature
	c := e.anonymous()
	_, out := passwordStep(e, c, "anna")
	if obj(out["methods"])["passkey"] != true || obj(out["methods"])["totp"] != false {
		t.Fatalf("%v", out)
	}
	token := out["token"]
	status, q := c.call("POST", "/api/login/passkey/begin", map[string]any{"token": token}, true)
	allow := list(q["allowCredentials"])
	if status != 200 || q["rpId"] != "pdu.example.com" || q["userVerification"] != "required" || len(allow) != 1 || obj(allow[0])["id"] != b64(key.id) {
		t.Fatalf("%d %v", status, q)
	}
	answer := key.assert(q, rpOrigin, 0x01|0x04)
	answer["token"] = token
	if status, _ := c.call("POST", "/api/login/passkey/finish", answer, true); status != 200 {
		t.Fatalf("login with the passkey: %d", status)
	}
	// the same answer cannot be sent again (the question is used up, the login is done)
	if status, _ := c.call("POST", "/api/login/passkey/finish", answer, true); status != 401 {
		t.Errorf("an answer twice: %d", status)
	}
	// a fingerprint that was not checked (no user verification) is not enough
	c2 := e.anonymous()
	_, out = passwordStep(e, c2, "anna")
	_, q = c2.call("POST", "/api/login/passkey/begin", map[string]any{"token": out["token"]}, true)
	weak := key.assert(q, rpOrigin, 0x01)
	weak["token"] = out["token"]
	if status, _ := c2.call("POST", "/api/login/passkey/finish", weak, true); status != 401 {
		t.Errorf("no user verification: %d", status)
	}
	// an answer to another question
	c3 := e.anonymous()
	_, out = passwordStep(e, c3, "anna")
	c3.call("POST", "/api/login/passkey/begin", map[string]any{"token": out["token"]}, true)
	stale := key.assert(map[string]any{"challenge": b64(webauthn.NewChallenge())}, rpOrigin, 0x05)
	stale["token"] = out["token"]
	if status, _ := c3.call("POST", "/api/login/passkey/finish", stale, true); status != 401 {
		t.Errorf("a wrong challenge: %d", status)
	}
	// a wrong answer uses up the question: the right one that follows is too late
	c5 := e.anonymous()
	_, out = passwordStep(e, c5, "anna")
	_, q = c5.call("POST", "/api/login/passkey/begin", map[string]any{"token": out["token"]}, true)
	good := key.assert(q, rpOrigin, 0x05)
	bad := key.assert(q, rpOrigin, 0x05)
	bad["signature"] = b64([]byte("not a signature"))
	bad["token"], good["token"] = out["token"], out["token"]
	if status, _ := c5.call("POST", "/api/login/passkey/finish", bad, true); status != 401 {
		t.Errorf("a bad signature: %d", status)
	}
	if status, _ := c5.call("POST", "/api/login/passkey/finish", good, true); status != 401 {
		t.Errorf("an answer to a question that was answered already: %d", status)
	}
	// a key that was never registered
	c4 := e.anonymous()
	_, out = passwordStep(e, c4, "anna")
	_, q = c4.call("POST", "/api/login/passkey/begin", map[string]any{"token": out["token"]}, true)
	stranger := newSoftKey(t, "pdu.example.com").assert(q, rpOrigin, 0x05)
	stranger["token"] = out["token"]
	if status, _ := c4.call("POST", "/api/login/passkey/finish", stranger, true); status != 401 {
		t.Errorf("a stranger: %d", status)
	}
	// the question needs a pending login
	if status, _ := e.anonymous().call("POST", "/api/login/passkey/begin", map[string]any{"token": "nope"}, true); status != 401 {
		t.Errorf("no login: %d", status)
	}
}
