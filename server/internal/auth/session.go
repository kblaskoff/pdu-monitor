package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kblaskoff/pdu-monitor/server/internal/store"
)

// Session is a person who is logged in. Sessions are kept in memory and, when a store is given, in the database too (only the hash of
// the token), so that a restart of the server (an update makes one) does not log everybody out.
type Session struct {
	User     *User
	CSRF     string
	Created  time.Time
	LastSeen time.Time
	IP       string
	MFA      bool // the second step was passed

	limited   atomic.Bool // only the setting up of the second step may be done
	persisted time.Time   // when LastSeen was last written to the database
}

// IsLimited is true for a session that may only set up the second step of the login (see Settings.RequireMFA).
func (s *Session) IsLimited() bool { return s.limited.Load() }

// UserKey names a person in the tables of the second step: the source and the lower case name, so that the emergency local account and
// a directory account with the same name are two people.
func UserKey(u *User) string { return u.Source + ":" + strings.ToLower(u.Name) }

// Sessions keeps the sessions. The cookie holds a random token; only its hash is kept here.
type Sessions struct {
	Idle time.Duration // logged out after this long without a request
	Max  time.Duration // logged out after this long in any case
	Now  func() time.Time
	// Store keeps the sessions across restarts; nil in tests that do not need it.
	Store *store.Store
	mu    sync.Mutex
	m     map[string]*Session
}

// How often the time of the last request is written to the database (not with every request).
const persistEvery = 5 * time.Minute

// MaxAge is how long a session lives at most (the lifetime of the cookie).
func (s *Sessions) MaxAge() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Max
}

// SetLimits changes how long sessions live (the setting of the administrator); it applies to the sessions that exist as well.
func (s *Sessions) SetLimits(idle, max time.Duration) {
	s.mu.Lock()
	s.Idle, s.Max = idle, max
	s.mu.Unlock()
}

func NewSessions(idle, max time.Duration) *Sessions {
	return &Sessions{Idle: idle, Max: max, Now: time.Now, m: map[string]*Session{}}
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err) // the system has no randomness: nothing safe can be done
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// Create starts a session and returns the token for the cookie.
func (s *Sessions) Create(u *User, ip string) (token string, sess *Session) {
	return s.CreateWith(u, ip, false, false)
}

// CreateWith is Create for a session that has passed the second step (mfa) or may only set it up (limited).
func (s *Sessions) CreateWith(u *User, ip string, mfa, limited bool) (token string, sess *Session) {
	token = randomToken()
	now := s.Now()
	sess = &Session{User: u, CSRF: randomToken(), Created: now, LastSeen: now, IP: ip, MFA: mfa, persisted: now}
	sess.limited.Store(limited)
	key := hashToken(token)
	s.mu.Lock()
	s.m[key] = sess
	s.mu.Unlock()
	s.save(key, sess)
	return token, sess
}

func (s *Sessions) save(key string, sess *Session) {
	if s.Store == nil {
		return
	}
	user, _ := json.Marshal(sess.User)
	_ = s.Store.SaveSession(context.Background(), store.StoredSession{Key: key, UserJSON: string(user), CSRF: sess.CSRF, Created: sess.Created,
		LastSeen: sess.LastSeen, IP: sess.IP, MFA: sess.MFA, Limited: sess.IsLimited()})
}

// Load brings back the sessions of before a restart (the ones that are still valid).
func (s *Sessions) Load(ctx context.Context) error {
	if s.Store == nil {
		return nil
	}
	now := s.Now()
	list, err := s.Store.LoadSessions(ctx, now.Add(-s.Idle))
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, x := range list {
		var u User
		if json.Unmarshal([]byte(x.UserJSON), &u) != nil || now.Sub(x.Created) > s.Max {
			continue
		}
		sess := &Session{User: &u, CSRF: x.CSRF, Created: x.Created, LastSeen: x.LastSeen, IP: x.IP, MFA: x.MFA, persisted: x.LastSeen}
		sess.limited.Store(x.Limited)
		s.m[x.Key] = sess
	}
	return nil
}

// Upgrade makes a limited session a full one (the person has set up the second step), as one that has passed it.
func (s *Sessions) Upgrade(token string) {
	key := hashToken(token)
	s.mu.Lock()
	sess := s.m[key]
	if sess != nil {
		sess.limited.Store(false)
		sess.MFA = true
	}
	s.mu.Unlock()
	if sess != nil && s.Store != nil {
		user, _ := json.Marshal(sess.User)
		_ = s.Store.SaveSession(context.Background(), store.StoredSession{Key: key, UserJSON: string(user), CSRF: sess.CSRF, Created: sess.Created,
			LastSeen: sess.LastSeen, IP: sess.IP, MFA: true, Limited: false})
	}
}

// DeleteUser logs a person out everywhere (their devices were reset).
func (s *Sessions) DeleteUser(userKey string) {
	s.mu.Lock()
	var gone []string
	for k, v := range s.m {
		if UserKey(v.User) == userKey {
			delete(s.m, k)
			gone = append(gone, k)
		}
	}
	s.mu.Unlock()
	if s.Store != nil {
		for _, k := range gone {
			_ = s.Store.DeleteSession(context.Background(), k)
		}
	}
}

// Get returns the session of a token (and counts the request as activity), or nil.
func (s *Sessions) Get(token string) *Session {
	if token == "" {
		return nil
	}
	key := hashToken(token)
	now := s.Now()
	s.mu.Lock()
	sess := s.m[key]
	if sess == nil {
		s.mu.Unlock()
		return nil
	}
	if now.Sub(sess.LastSeen) > s.Idle || now.Sub(sess.Created) > s.Max {
		delete(s.m, key)
		s.mu.Unlock()
		return nil
	}
	sess.LastSeen = now
	touch := s.Store != nil && now.Sub(sess.persisted) >= persistEvery
	if touch {
		sess.persisted = now
	}
	s.mu.Unlock()
	if touch {
		_ = s.Store.TouchSession(context.Background(), key, now)
	}
	return sess
}

func (s *Sessions) Delete(token string) {
	key := hashToken(token)
	s.mu.Lock()
	delete(s.m, key)
	s.mu.Unlock()
	if s.Store != nil {
		_ = s.Store.DeleteSession(context.Background(), key)
	}
}

// Reap removes the sessions that have expired.
func (s *Sessions) Reap() {
	now := s.Now()
	s.mu.Lock()
	for k, v := range s.m {
		if now.Sub(v.LastSeen) > s.Idle || now.Sub(v.Created) > s.Max {
			delete(s.m, k)
		}
	}
	idle := s.Idle
	s.mu.Unlock()
	if s.Store != nil {
		_ = s.Store.PruneSessions(context.Background(), now.Add(-idle))
	}
}

func (s *Sessions) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.m)
}

// CheckCSRF compares the token of a request with the one of the session in constant time.
func (sess *Session) CheckCSRF(token string) bool {
	return token != "" && subtle.ConstantTimeCompare([]byte(token), []byte(sess.CSRF)) == 1
}

// Limiter slows down guessing: after Max failures in Window a key is locked until the oldest failure is out of the window.
type Limiter struct {
	Max    int
	Window time.Duration
	Now    func() time.Time
	mu     sync.Mutex
	fails  map[string][]time.Time
}

func NewLimiter(max int, window time.Duration) *Limiter {
	return &Limiter{Max: max, Window: window, Now: time.Now, fails: map[string][]time.Time{}}
}

func (l *Limiter) prune(key string, now time.Time) []time.Time {
	var keep []time.Time
	for _, t := range l.fails[key] {
		if now.Sub(t) < l.Window {
			keep = append(keep, t)
		}
	}
	if len(keep) == 0 {
		delete(l.fails, key)
	} else {
		l.fails[key] = keep
	}
	return keep
}

// Allow is false while the key is locked; retry says for how long.
func (l *Limiter) Allow(key string) (ok bool, retry time.Duration) {
	now := l.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	f := l.prune(key, now)
	if len(f) < l.Max {
		return true, 0
	}
	return false, l.Window - now.Sub(f[0])
}

func (l *Limiter) Fail(key string) {
	now := l.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.prune(key, now)
	l.fails[key] = append(l.fails[key], now)
}

func (l *Limiter) Reset(key string) {
	l.mu.Lock()
	delete(l.fails, key)
	l.mu.Unlock()
}
