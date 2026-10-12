package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"sync"
	"time"
)

// Session is a person who is logged in (kept in memory: a restart of the server logs everybody out).
type Session struct {
	User     *User
	CSRF     string
	Created  time.Time
	LastSeen time.Time
	IP       string
}

// Sessions keeps the sessions. The cookie holds a random token; only its hash is kept here.
type Sessions struct {
	Idle time.Duration // logged out after this long without a request
	Max  time.Duration // logged out after this long in any case
	Now  func() time.Time
	mu   sync.Mutex
	m    map[string]*Session
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
	token = randomToken()
	now := s.Now()
	sess = &Session{User: u, CSRF: randomToken(), Created: now, LastSeen: now, IP: ip}
	s.mu.Lock()
	s.m[hashToken(token)] = sess
	s.mu.Unlock()
	return token, sess
}

// Get returns the session of a token (and counts the request as activity), or nil.
func (s *Sessions) Get(token string) *Session {
	if token == "" {
		return nil
	}
	key := hashToken(token)
	now := s.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.m[key]
	if sess == nil {
		return nil
	}
	if now.Sub(sess.LastSeen) > s.Idle || now.Sub(sess.Created) > s.Max {
		delete(s.m, key)
		return nil
	}
	sess.LastSeen = now
	return sess
}

func (s *Sessions) Delete(token string) {
	s.mu.Lock()
	delete(s.m, hashToken(token))
	s.mu.Unlock()
}

// Reap removes the sessions that have expired.
func (s *Sessions) Reap() {
	now := s.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range s.m {
		if now.Sub(v.LastSeen) > s.Idle || now.Sub(v.Created) > s.Max {
			delete(s.m, k)
		}
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
