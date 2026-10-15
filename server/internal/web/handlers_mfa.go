package web

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"sync"
	"time"

	"rsc.io/qr"

	"github.com/kblaskoff/pdu-monitor/server/internal/auth"
	"github.com/kblaskoff/pdu-monitor/server/internal/store"
	"github.com/kblaskoff/pdu-monitor/server/internal/totp"
	"github.com/kblaskoff/pdu-monitor/server/internal/webauthn"
)

// The second step of the login: an authenticator app (TOTP), a passkey (a fingerprint on a laptop, a phone, a security key) or a
// recovery code. The password is checked first; when the person has a second step, no session is made yet: the answer holds a token
// for a few minutes that only the second step can turn into a session.

const (
	mfaPendingTTL = 5 * time.Minute
	mfaMaxPending = 2000
	mfaMaxFails   = 5 // wrong codes for one password check; then the password is asked again
	recoveryCount = 10
)

type mfaPending struct {
	user      *auth.User
	ip        string
	expires   time.Time
	fails     int
	challenge []byte // of the passkey question that is open
}

type enrollment struct {
	kind      string
	name      string
	secret    []byte
	challenge []byte
	expires   time.Time
}

type mfaState struct {
	mu      sync.Mutex
	logins  map[string]*mfaPending
	enrolls map[*auth.Session]*enrollment
}

func newMFAState() *mfaState {
	return &mfaState{logins: map[string]*mfaPending{}, enrolls: map[*auth.Session]*enrollment{}}
}

func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return webauthn.B64(b)
}

func (m *mfaState) newLogin(u *auth.User, ip string, now time.Time) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, v := range m.logins { // forget the old ones; and never keep more than a limit
		if now.After(v.expires) {
			delete(m.logins, k)
		}
	}
	if len(m.logins) >= mfaMaxPending {
		return ""
	}
	token := randomToken()
	m.logins[token] = &mfaPending{user: u, ip: ip, expires: now.Add(mfaPendingTTL)}
	return token
}

// login returns a copy of the pending login, or nil.
func (m *mfaState) login(token string, now time.Time) *mfaPending {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.logins[token]
	if p == nil || now.After(p.expires) {
		delete(m.logins, token)
		return nil
	}
	c := *p
	return &c
}

func (m *mfaState) fail(token string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p := m.logins[token]; p != nil {
		p.fails++
		if p.fails >= mfaMaxFails {
			delete(m.logins, token)
		}
	}
}

func (m *mfaState) done(token string) {
	m.mu.Lock()
	delete(m.logins, token)
	m.mu.Unlock()
}

func (m *mfaState) setChallenge(token string, c []byte) {
	m.mu.Lock()
	if p := m.logins[token]; p != nil {
		p.challenge = c
	}
	m.mu.Unlock()
}

// takeChallenge returns the open challenge and forgets it: an answer can be given once.
func (m *mfaState) takeChallenge(token string) []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p := m.logins[token]; p != nil {
		c := p.challenge
		p.challenge = nil
		return c
	}
	return nil
}

func (m *mfaState) setEnroll(sess *auth.Session, e *enrollment) {
	m.mu.Lock()
	m.enrolls[sess] = e
	m.mu.Unlock()
}

func (m *mfaState) takeEnroll(sess *auth.Session, kind string, now time.Time) *enrollment {
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.enrolls[sess]
	delete(m.enrolls, sess)
	if e == nil || e.kind != kind || now.After(e.expires) {
		return nil
	}
	return e
}

// peekEnroll is takeEnroll for a code that may be wrong: the person can try again until it expires.
func (m *mfaState) peekEnroll(sess *auth.Session, kind string, now time.Time) *enrollment {
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.enrolls[sess]
	if e == nil || e.kind != kind || now.After(e.expires) {
		return nil
	}
	return e
}

func (m *mfaState) dropEnroll(sess *auth.Session) {
	m.mu.Lock()
	delete(m.enrolls, sess)
	m.mu.Unlock()
}

// ---- recovery codes

const recoveryAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789" // no 0 O 1 I

func newRecoveryCodes() (codes []string, hashes []string) {
	for i := 0; i < recoveryCount; i++ {
		b := make([]byte, 10)
		_, _ = rand.Read(b)
		for j := range b {
			b[j] = recoveryAlphabet[int(b[j])%len(recoveryAlphabet)] // 256 is a multiple of 32: no bias
		}
		code := string(b)
		codes = append(codes, code[:5]+"-"+code[5:])
		hashes = append(hashes, recoveryHash(code))
	}
	return codes, hashes
}

func recoveryHash(code string) string {
	code = strings.ToUpper(strings.NewReplacer("-", "", " ", "").Replace(strings.TrimSpace(code)))
	sum := sha256.Sum256([]byte("pdu-monitor recovery " + code))
	return hex.EncodeToString(sum[:])
}

// ---- the second step of the login

type mfaMethods struct {
	TOTP     bool `json:"totp"`
	Passkey  bool `json:"passkey"`
	Recovery bool `json:"recovery"`
}

func (s *Server) methodsOf(ctx context.Context, key string, factors []store.Factor) mfaMethods {
	var m mfaMethods
	for _, f := range factors {
		switch f.Kind {
		case store.FactorTOTP:
			m.TOTP = true
		case store.FactorPasskey:
			m.Passkey = s.RP != nil
		}
	}
	n, _ := s.Store.RecoveryLeft(ctx, key)
	m.Recovery = n > 0
	return m
}

type mfaLoginRequest struct {
	Token  string `json:"token"`
	Method string `json:"method"` // totp, recovery
	Code   string `json:"code"`
}

// mfaGate is the common start of the three answers: the origin, the limits and the pending login.
func (s *Server) mfaGate(w http.ResponseWriter, r *http.Request, token string) (*mfaPending, string, bool) {
	if !sameOrigin(r) {
		writeError(w, http.StatusForbidden, "the request was refused")
		return nil, "", false
	}
	ip := remoteIP(r)
	if ok, retry := s.byIP.Allow("ip:" + ip); !ok {
		w.Header().Set("Retry-After", itoa(int(retry.Seconds())+1))
		writeError(w, http.StatusTooManyRequests, "too many attempts: wait a few minutes")
		return nil, "", false
	}
	p := s.mfa.login(token, s.Now())
	if p == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "the login has expired: enter the password again", "expired": true})
		return nil, "", false
	}
	key := "mfa:" + auth.UserKey(p.user)
	if ok, retry := s.byMFA.Allow(key); !ok {
		w.Header().Set("Retry-After", itoa(int(retry.Seconds())+1))
		writeError(w, http.StatusTooManyRequests, "too many wrong codes: wait a few minutes")
		return nil, "", false
	}
	return p, key, true
}

func (s *Server) mfaWrong(w http.ResponseWriter, r *http.Request, p *mfaPending, token, key, how string) {
	s.mfa.fail(token)
	s.byMFA.Fail(key)
	s.byIP.Fail("ip:" + remoteIP(r))
	_ = s.Store.Audit(r.Context(), p.user.Name, "login.mfa.failed", how, false)
	time.Sleep(s.FailureDelay)
	writeError(w, http.StatusUnauthorized, "that is not right")
}

func (s *Server) mfaPassed(w http.ResponseWriter, r *http.Request, p *mfaPending, token, key, how string) {
	s.mfa.done(token)
	s.byMFA.Reset(key)
	s.startSession(w, r.Context(), p.user, remoteIP(r), true, false, ", second step: "+how)
}

// handleLoginMFA takes a code of an authenticator app, or a recovery code.
func (s *Server) handleLoginMFA(w http.ResponseWriter, r *http.Request) {
	var req mfaLoginRequest
	if !readJSON(w, r, &req) {
		return
	}
	p, key, ok := s.mfaGate(w, r, req.Token)
	if !ok {
		return
	}
	ctx := r.Context()
	ukey := auth.UserKey(p.user)
	switch req.Method {
	case "totp":
		factors, err := s.Store.Factors(ctx, ukey)
		if err != nil {
			writeError(w, 500, err.Error())
			return
		}
		for _, f := range factors {
			if f.Kind != store.FactorTOTP {
				continue
			}
			if step, ok := totp.Verify(f.Secret, req.Code, s.Now(), f.LastStep); ok {
				if used, _ := s.Store.UseTOTPStep(ctx, f.ID, step); used {
					s.mfaPassed(w, r, p, req.Token, key, "authenticator app “"+f.Name+"”")
					return
				}
			}
		}
		s.mfaWrong(w, r, p, req.Token, key, "authenticator code")
	case "recovery":
		if used, _ := s.Store.UseRecoveryCode(ctx, ukey, recoveryHash(req.Code)); used {
			left, _ := s.Store.RecoveryLeft(ctx, ukey)
			_ = s.Store.Audit(ctx, p.user.Name, "login.recovery", itoa(left)+" recovery codes are left", true)
			s.mfaPassed(w, r, p, req.Token, key, "a recovery code")
			return
		}
		s.mfaWrong(w, r, p, req.Token, key, "recovery code")
	default:
		writeError(w, http.StatusBadRequest, "unknown method")
	}
}

type credentialDescriptor struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// handleLoginPasskeyBegin starts the question to a passkey.
func (s *Server) handleLoginPasskeyBegin(w http.ResponseWriter, r *http.Request) {
	var req mfaLoginRequest
	if !readJSON(w, r, &req) {
		return
	}
	p, _, ok := s.mfaGate(w, r, req.Token)
	if !ok {
		return
	}
	if s.RP == nil {
		writeError(w, http.StatusConflict, "passkeys are not set up on this server (public_url in config.yaml)")
		return
	}
	factors, err := s.Store.Factors(r.Context(), auth.UserKey(p.user))
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	var allow []credentialDescriptor
	for _, f := range factors {
		if f.Kind == store.FactorPasskey {
			allow = append(allow, credentialDescriptor{"public-key", webauthn.B64(f.CredID)})
		}
	}
	if len(allow) == 0 {
		writeError(w, http.StatusConflict, "this account has no passkey")
		return
	}
	challenge := webauthn.NewChallenge()
	s.mfa.setChallenge(req.Token, challenge)
	writeJSON(w, 200, map[string]any{"challenge": webauthn.B64(challenge), "rpId": s.RP.ID, "timeout": 120000, "userVerification": "required", "allowCredentials": allow})
}

type passkeyAnswer struct {
	Token             string `json:"token"`
	ID                string `json:"id"`
	ClientDataJSON    string `json:"clientDataJSON"`
	AuthenticatorData string `json:"authenticatorData"`
	Signature         string `json:"signature"`
	AttestationObject string `json:"attestationObject"`
}

// handleLoginPasskeyFinish checks the answer of a passkey.
func (s *Server) handleLoginPasskeyFinish(w http.ResponseWriter, r *http.Request) {
	var req passkeyAnswer
	if !readJSON(w, r, &req) {
		return
	}
	p, key, ok := s.mfaGate(w, r, req.Token)
	if !ok {
		return
	}
	challenge := s.mfa.takeChallenge(req.Token)
	id, e1 := webauthn.Decode(req.ID)
	cd, e2 := webauthn.Decode(req.ClientDataJSON)
	ad, e3 := webauthn.Decode(req.AuthenticatorData)
	sig, e4 := webauthn.Decode(req.Signature)
	if s.RP == nil || challenge == nil || e1 != nil || e2 != nil || e3 != nil || e4 != nil {
		s.mfaWrong(w, r, p, req.Token, key, "passkey (no question or a broken answer)")
		return
	}
	factors, err := s.Store.Factors(r.Context(), auth.UserKey(p.user))
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	for _, f := range factors {
		if f.Kind != store.FactorPasskey || string(f.CredID) != string(id) {
			continue
		}
		count, err := s.RP.VerifyAssertion(challenge, f.PublicKey, f.SignCount, cd, ad, sig)
		if err != nil {
			s.logf("passkey of " + p.user.Name + ": " + err.Error())
			break
		}
		if used, _ := s.Store.UsePasskeyCount(r.Context(), f.ID, count); !used {
			break
		}
		s.mfaPassed(w, r, p, req.Token, key, "passkey “"+f.Name+"”")
		return
	}
	s.mfaWrong(w, r, p, req.Token, key, "passkey")
}

// ---- setting up (a person who is logged in; a limited session may do only this)

type factorView struct {
	ID       int64      `json:"id"`
	Kind     string     `json:"kind"`
	Name     string     `json:"name"`
	Created  time.Time  `json:"created"`
	LastUsed *time.Time `json:"lastUsed,omitempty"`
}

func (s *Server) mfaSummary(ctx context.Context, sess *auth.Session) (map[string]any, error) {
	key := auth.UserKey(sess.User)
	factors, err := s.Store.Factors(ctx, key)
	if err != nil {
		return nil, err
	}
	views := make([]factorView, 0, len(factors))
	for _, f := range factors {
		v := factorView{ID: f.ID, Kind: f.Kind, Name: f.Name, Created: f.Created}
		if !f.LastUsed.IsZero() {
			t := f.LastUsed
			v.LastUsed = &t
		}
		views = append(views, v)
	}
	left, _ := s.Store.RecoveryLeft(ctx, key)
	st, _ := s.Store.Settings(ctx)
	return map[string]any{"factors": views, "recovery": left, "require": st.RequireMFA && sess.User.Source != "local", "passkeys": s.RP != nil,
		"max": store.MaxFactorsPerKind, "limited": sess.IsLimited()}, nil
}

func (s *Server) handleMFAGet(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
	out, err := s.mfaSummary(r.Context(), sess)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, out)
}

type nameRequest struct {
	Name string `json:"name"`
	Code string `json:"code"`
}

func cleanDeviceName(name string) (string, bool) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 40 {
		return "", false
	}
	for _, c := range name {
		if c < ' ' || c == 0x7f {
			return "", false
		}
	}
	return name, true
}

const enrollTTL = 10 * time.Minute

func (s *Server) countOf(ctx context.Context, key, kind string) int {
	n := 0
	fs, _ := s.Store.Factors(ctx, key)
	for _, f := range fs {
		if f.Kind == kind {
			n++
		}
	}
	return n
}

// handleTOTPBegin makes a secret and shows it (as text and as a QR code); nothing is kept until a code from the app is confirmed.
func (s *Server) handleTOTPBegin(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
	var req nameRequest
	if !readJSON(w, r, &req) {
		return
	}
	name, ok := cleanDeviceName(req.Name)
	if !ok {
		writeError(w, http.StatusBadRequest, "give the device a name (up to 40 characters), for example “phone”")
		return
	}
	if s.countOf(r.Context(), auth.UserKey(sess.User), store.FactorTOTP) >= store.MaxFactorsPerKind {
		writeError(w, http.StatusConflict, store.ErrTooManyFactors.Error())
		return
	}
	secret := totp.NewSecret()
	s.mfa.setEnroll(sess, &enrollment{kind: store.FactorTOTP, name: name, secret: secret, expires: s.Now().Add(enrollTTL)})
	uri := totp.URI("PDU Monitor", sess.User.Name, secret)
	code, err := qr.Encode(uri, qr.M)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	rows := make([]string, code.Size)
	for y := 0; y < code.Size; y++ {
		var b strings.Builder
		for x := 0; x < code.Size; x++ {
			if code.Black(x, y) {
				b.WriteByte('1')
			} else {
				b.WriteByte('0')
			}
		}
		rows[y] = b.String()
	}
	writeJSON(w, 200, map[string]any{"secret": totp.EncodeSecret(secret), "uri": uri, "qr": rows})
}

// afterFirstFactor makes the recovery codes (when there are none), and lets a limited session do everything.
func (s *Server) afterFirstFactor(r *http.Request, sess *auth.Session) []string {
	ctx := r.Context()
	key := auth.UserKey(sess.User)
	var codes []string
	if n, _ := s.Store.RecoveryLeft(ctx, key); n == 0 {
		var hashes []string
		codes, hashes = newRecoveryCodes()
		if err := s.Store.ReplaceRecoveryCodes(ctx, key, hashes); err != nil {
			return nil
		}
	}
	if sess.IsLimited() {
		if cookie, err := r.Cookie(cookieName); err == nil {
			s.Sessions.Upgrade(cookie.Value)
		}
	}
	return codes
}

// handleTOTPConfirm keeps the secret once the app shows the right code (so that a secret that was not scanned is never relied on).
func (s *Server) handleTOTPConfirm(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
	var req nameRequest
	if !readJSON(w, r, &req) {
		return
	}
	e := s.mfa.peekEnroll(sess, store.FactorTOTP, s.Now())
	if e == nil {
		writeError(w, http.StatusConflict, "this setting up has expired: start again")
		return
	}
	step, ok := totp.Verify(e.secret, req.Code, s.Now(), 0)
	if !ok {
		time.Sleep(s.FailureDelay)
		writeError(w, http.StatusUnauthorized, "that code is not right: check the time of the phone and try the next code")
		return
	}
	s.mfa.dropEnroll(sess)
	id, err := s.Store.AddTOTP(r.Context(), auth.UserKey(sess.User), e.name, e.secret)
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	_, _ = s.Store.UseTOTPStep(r.Context(), id, step) // this code was seen: it cannot open a login
	_ = s.Store.Audit(r.Context(), sess.User.Name, "mfa.totp.added", e.name, true)
	codes := s.afterFirstFactor(r, sess)
	out, _ := s.mfaSummary(r.Context(), sess)
	out["recoveryCodes"] = codes
	writeJSON(w, 200, out)
}

// handlePasskeyBegin starts the making of a passkey.
func (s *Server) handlePasskeyBegin(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
	var req nameRequest
	if !readJSON(w, r, &req) {
		return
	}
	if s.RP == nil {
		writeError(w, http.StatusConflict, "passkeys are not set up on this server: add public_url (https://the-name-of-this-server) to config.yaml and restart it")
		return
	}
	name, ok := cleanDeviceName(req.Name)
	if !ok {
		writeError(w, http.StatusBadRequest, "give the device a name (up to 40 characters), for example “laptop”")
		return
	}
	key := auth.UserKey(sess.User)
	factors, err := s.Store.Factors(r.Context(), key)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	var exclude []credentialDescriptor
	n := 0
	for _, f := range factors {
		if f.Kind == store.FactorPasskey {
			n++
			exclude = append(exclude, credentialDescriptor{"public-key", webauthn.B64(f.CredID)})
		}
	}
	if n >= store.MaxFactorsPerKind {
		writeError(w, http.StatusConflict, store.ErrTooManyFactors.Error())
		return
	}
	challenge := webauthn.NewChallenge()
	s.mfa.setEnroll(sess, &enrollment{kind: store.FactorPasskey, name: name, challenge: challenge, expires: s.Now().Add(enrollTTL)})
	handle := sha256.Sum256([]byte("pdu-monitor user " + key))
	display := sess.User.DisplayName
	if display == "" {
		display = sess.User.Name
	}
	writeJSON(w, 200, map[string]any{
		"challenge": webauthn.B64(challenge), "rp": map[string]string{"id": s.RP.ID, "name": "PDU Monitor"},
		"user":             map[string]string{"id": webauthn.B64(handle[:]), "name": sess.User.Name, "displayName": display},
		"pubKeyCredParams": []map[string]any{{"type": "public-key", "alg": -7}, {"type": "public-key", "alg": -8}, {"type": "public-key", "alg": -257}},
		"timeout":          120000, "attestation": "none", "excludeCredentials": exclude,
		"authenticatorSelection": map[string]string{"residentKey": "preferred", "userVerification": "required"},
	})
}

func (s *Server) handlePasskeyFinish(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
	var req passkeyAnswer
	if !readJSON(w, r, &req) {
		return
	}
	e := s.mfa.takeEnroll(sess, store.FactorPasskey, s.Now())
	cd, e1 := webauthn.Decode(req.ClientDataJSON)
	att, e2 := webauthn.Decode(req.AttestationObject)
	if s.RP == nil || e == nil || e1 != nil || e2 != nil {
		writeError(w, http.StatusConflict, "this setting up has expired or the answer is broken: start again")
		return
	}
	reg, err := s.RP.ParseRegistration(e.challenge, cd, att)
	if err != nil {
		writeError(w, http.StatusBadRequest, "the passkey was not accepted: "+err.Error())
		return
	}
	if _, err := s.Store.AddPasskey(r.Context(), auth.UserKey(sess.User), e.name, reg.CredentialID, reg.PublicKey, reg.SignCount); err != nil {
		msg := err.Error()
		if strings.Contains(msg, "UNIQUE") {
			msg = "this passkey is added already"
		}
		writeError(w, http.StatusConflict, msg)
		return
	}
	_ = s.Store.Audit(r.Context(), sess.User.Name, "mfa.passkey.added", e.name, true)
	codes := s.afterFirstFactor(r, sess)
	out, _ := s.mfaSummary(r.Context(), sess)
	out["recoveryCodes"] = codes
	writeJSON(w, 200, out)
}

func (s *Server) handleMFARemove(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusNotFound, "no such device")
		return
	}
	ctx := r.Context()
	key := auth.UserKey(sess.User)
	factors, _ := s.Store.Factors(ctx, key)
	st, _ := s.Store.Settings(ctx)
	if st.RequireMFA && sess.User.Source != "local" && len(factors) <= 1 {
		writeError(w, http.StatusConflict, "the administrator requires a second step: add another device before you remove the last one")
		return
	}
	removed, err := s.Store.RemoveFactor(ctx, key, id)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if !removed {
		writeError(w, http.StatusNotFound, "no such device")
		return
	}
	if len(factors) <= 1 {
		_ = s.Store.ReplaceRecoveryCodes(ctx, key, nil) // no devices: the recovery codes mean nothing
	}
	_ = s.Store.Audit(ctx, sess.User.Name, "mfa.removed", itoa(int(id)), true)
	out, _ := s.mfaSummary(ctx, sess)
	writeJSON(w, 200, out)
}

// handleRecoveryNew makes new recovery codes; the old ones stop working.
func (s *Server) handleRecoveryNew(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
	ctx := r.Context()
	key := auth.UserKey(sess.User)
	if fs, _ := s.Store.Factors(ctx, key); len(fs) == 0 {
		writeError(w, http.StatusConflict, "add an authenticator app or a passkey first")
		return
	}
	codes, hashes := newRecoveryCodes()
	if err := s.Store.ReplaceRecoveryCodes(ctx, key, hashes); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	_ = s.Store.Audit(ctx, sess.User.Name, "mfa.recovery.new", "", true)
	out, _ := s.mfaSummary(ctx, sess)
	out["recoveryCodes"] = codes
	writeJSON(w, 200, out)
}

// ---- the administrator

func (s *Server) handleAdminMFA(w http.ResponseWriter, r *http.Request, _ *auth.Session) {
	users, err := s.Store.UsersWithFactors(r.Context())
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"users": nonNil(users)})
}

// handleAdminMFAReset removes all the devices of somebody who lost them, and logs them out everywhere.
func (s *Server) handleAdminMFAReset(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
	user := r.URL.Query().Get("user")
	if user == "" || len(user) > 200 {
		writeError(w, http.StatusBadRequest, "which person?")
		return
	}
	n, err := s.Store.ResetFactors(r.Context(), user)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	s.Sessions.DeleteUser(user)
	_ = s.Store.Audit(r.Context(), sess.User.Name, "mfa.reset", user+": "+itoa(n)+" devices", true)
	writeJSON(w, 200, map[string]any{"removed": n})
}
