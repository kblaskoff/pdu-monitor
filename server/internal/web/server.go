// Package web is the HTTP interface: a JSON API and the single page that uses it.
package web

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/kblaskoff/pdu-monitor/server/internal/app"
	"github.com/kblaskoff/pdu-monitor/server/internal/auth"
	"github.com/kblaskoff/pdu-monitor/server/internal/store"
)

//go:embed static
var staticFiles embed.FS

// staticTags is the ETag of every embedded file (a hash of its content), made once when the program starts.
var staticTags = func() map[string]string {
	tags := map[string]string{}
	_ = fs.WalkDir(staticFiles, "static", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if data, err := staticFiles.ReadFile(path); err == nil {
			sum := sha256.Sum256(data)
			tags[strings.TrimPrefix(path, "static/")] = `"` + hex.EncodeToString(sum[:8]) + `"`
		}
		return nil
	})
	return tags
}()

// KeyFetcher looks at the SSH key a host offers (the real one is hostinfo.SSHClient).
type KeyFetcher interface {
	FetchHostKey(ctx context.Context, addr string, port int) (ssh.PublicKey, error)
}

const cookieName = "pdum_session"

// Server is the web application.
type Server struct {
	App      *app.App
	Store    *store.Store
	Auth     auth.Authenticator
	Sessions *auth.Sessions
	Keys     KeyFetcher
	// ServerKey is the public key of this server that the hosts must accept (shown to administrators).
	ServerKey string
	// Secure marks the cookie "Secure" (TLS is used).
	Secure  bool
	Demo    bool
	Version string
	// Collectors may push the readings of hypervisors (POST /api/collector/report) with their token.
	Collectors []CollectorAuth
	Log        func(string)
	Now        func() time.Time

	byIP   *auth.Limiter
	byUser *auth.Limiter
	// FailureDelay slows down every failed login a little (tests set it to 0).
	FailureDelay time.Duration

	pendingKeys *pendingKeys
	collected   *collectors
}

func New(a *app.App, st *store.Store, authn auth.Authenticator, sessions *auth.Sessions) *Server {
	return &Server{App: a, Store: st, Auth: authn, Sessions: sessions, Now: time.Now, Version: "dev",
		byIP: auth.NewLimiter(20, 10*time.Minute), byUser: auth.NewLimiter(6, 15*time.Minute), FailureDelay: 400 * time.Millisecond,
		pendingKeys: newPendingKeys(), collected: &collectors{seen: map[string]collectorSeen{}}}
}

func (s *Server) logf(msg string) {
	if s.Log != nil {
		s.Log(msg)
	}
}

// Handler is the whole site.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("POST /api/logout", s.api(auth.RoleViewer, s.handleLogout))
	mux.HandleFunc("GET /api/session", s.handleSession)
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"ok": true, "version": s.Version})
	})

	mux.HandleFunc("POST /api/collector/report", s.handleCollectorReport)

	mux.HandleFunc("GET /api/overview", s.api(auth.RoleViewer, s.handleOverview))
	mux.HandleFunc("GET /api/racks/{id}", s.api(auth.RoleViewer, s.handleRack))
	mux.HandleFunc("PUT /api/racks/{id}/labels", s.api(auth.RoleOperator, s.handleLabel))
	mux.HandleFunc("GET /api/pdus/{id}", s.api(auth.RoleViewer, s.handlePDU))
	mux.HandleFunc("GET /api/pdus/{id}/history", s.api(auth.RoleViewer, s.handlePDUHistory))
	mux.HandleFunc("GET /api/hosts", s.api(auth.RoleViewer, s.handleHosts))
	mux.HandleFunc("GET /api/hosts/{id}", s.api(auth.RoleViewer, s.handleHost))
	mux.HandleFunc("GET /api/hosts/{id}/top", s.api(auth.RoleViewer, s.handleTop))
	mux.HandleFunc("POST /api/power", s.api(auth.RoleOperator, s.handlePower))
	mux.HandleFunc("GET /api/ops", s.api(auth.RoleViewer, s.handleOps))
	mux.HandleFunc("GET /api/ops/{id}", s.api(auth.RoleViewer, s.handleOp))

	mux.HandleFunc("GET /api/admin/config", s.api(auth.RoleAdmin, s.handleAdminConfig))
	mux.HandleFunc("POST /api/admin/racks", s.api(auth.RoleAdmin, s.handleRackSave))
	mux.HandleFunc("PUT /api/admin/racks/{id}", s.api(auth.RoleAdmin, s.handleRackSave))
	mux.HandleFunc("DELETE /api/admin/racks/{id}", s.api(auth.RoleAdmin, s.handleRackDelete))
	mux.HandleFunc("POST /api/admin/pdus", s.api(auth.RoleAdmin, s.handlePDUSave))
	mux.HandleFunc("PUT /api/admin/pdus/{id}", s.api(auth.RoleAdmin, s.handlePDUSave))
	mux.HandleFunc("DELETE /api/admin/pdus/{id}", s.api(auth.RoleAdmin, s.handlePDUDelete))
	mux.HandleFunc("POST /api/admin/pdus/test", s.api(auth.RoleAdmin, s.handlePDUTest))
	mux.HandleFunc("POST /api/admin/hosts", s.api(auth.RoleAdmin, s.handleHostSave))
	mux.HandleFunc("PUT /api/admin/hosts/{id}", s.api(auth.RoleAdmin, s.handleHostSave))
	mux.HandleFunc("DELETE /api/admin/hosts/{id}", s.api(auth.RoleAdmin, s.handleHostDelete))
	mux.HandleFunc("POST /api/admin/hosts/{id}/fetchkey", s.api(auth.RoleAdmin, s.handleHostFetchKey))
	mux.HandleFunc("POST /api/admin/hosts/{id}/trust", s.api(auth.RoleAdmin, s.handleHostTrust))
	mux.HandleFunc("GET /api/admin/settings", s.api(auth.RoleAdmin, s.handleSettingsGet))
	mux.HandleFunc("PUT /api/admin/settings", s.api(auth.RoleAdmin, s.handleSettingsPut))
	mux.HandleFunc("POST /api/admin/import", s.api(auth.RoleAdmin, s.handleImport))
	mux.HandleFunc("GET /api/admin/audit", s.api(auth.RoleAdmin, s.handleAudit))
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) { writeError(w, http.StatusNotFound, "no such call") })

	sub, _ := fs.Sub(staticFiles, "static")
	files := http.FileServer(http.FS(sub))
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Path != "/" && !strings.HasPrefix(r.URL.Path, "/static/") && !strings.Contains(r.URL.Path, ".") {
			http.NotFound(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/static/") { // the files live under /static/ in the page, but at the root of the embedded tree
			r2 := r.Clone(r.Context())
			r2.URL.Path = strings.TrimPrefix(r.URL.Path, "/static")
			r = r2
		}
		// The files of the page are checked with the server at every load: a 304 with a few bytes when nothing changed, the new file
		// after an update. Without this a browser keeps the old script and a new server looks unchanged until a forced reload.
		name := strings.TrimPrefix(r.URL.Path, "/")
		if name == "" {
			name = "index.html"
		}
		if tag, ok := staticTags[name]; ok {
			w.Header().Set("ETag", tag)
		}
		w.Header().Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	}))
	return s.secure(mux)
}

// secure adds the headers every answer gets.
func (s *Server) secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		if s.Secure {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			h.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

// ---- helpers

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<20)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "the request is not valid JSON")
		return false
	}
	return true
}

func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func pathID(r *http.Request) (int64, bool) {
	var id int64
	for _, c := range r.PathValue("id") {
		if c < '0' || c > '9' || id > 1<<40 {
			return 0, false
		}
		id = id*10 + int64(c-'0')
	}
	return id, r.PathValue("id") != ""
}

// sameOrigin refuses requests that a page of another site made with the person's cookie.
func sameOrigin(r *http.Request) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
		return false
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		if err != nil || u.Host != r.Host {
			return false
		}
	}
	return true
}

type handlerFunc func(w http.ResponseWriter, r *http.Request, sess *auth.Session)

// api wraps a handler: a valid session, a role that may do it and, for anything that changes something, the CSRF token.
func (s *Server) api(min auth.Role, h handlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(cookieName)
		var sess *auth.Session
		if err == nil {
			sess = s.Sessions.Get(cookie.Value)
		}
		if sess == nil {
			writeError(w, http.StatusUnauthorized, "log in first")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if !sameOrigin(r) || !sess.CheckCSRF(r.Header.Get("X-CSRF-Token")) {
				writeError(w, http.StatusForbidden, "the request was refused (reload the page and try again)")
				return
			}
		}
		if !sess.User.Role.Can(min) {
			writeError(w, http.StatusForbidden, "your account may not do this")
			return
		}
		h(w, r, sess)
	}
}

// ---- login

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (s *Server) sessionInfo(sess *auth.Session) map[string]any {
	st, _ := s.Store.Settings(context.Background())
	return map[string]any{"user": sess.User, "csrf": sess.CSRF, "demo": s.Demo, "version": s.Version,
		"settings": map[string]any{"warnPercent": st.WarnPercent, "restartDelaySeconds": st.RestartDelaySecond, "pollSeconds": st.PollSeconds}}
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(cookieName); err == nil {
		if sess := s.Sessions.Get(cookie.Value); sess != nil {
			writeJSON(w, 200, s.sessionInfo(sess))
			return
		}
	}
	writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "log in first", "demo": s.Demo, "version": s.Version})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		writeError(w, http.StatusForbidden, "the request was refused")
		return
	}
	var req loginRequest
	if !readJSON(w, r, &req) {
		return
	}
	ip := remoteIP(r)
	userKey := "user:" + strings.ToLower(req.Username)
	ipKey := "ip:" + ip
	if ok, retry := s.byIP.Allow(ipKey); !ok {
		w.Header().Set("Retry-After", itoa(int(retry.Seconds())+1))
		writeError(w, http.StatusTooManyRequests, "too many attempts: wait a few minutes")
		return
	}
	if ok, retry := s.byUser.Allow(userKey); !ok {
		w.Header().Set("Retry-After", itoa(int(retry.Seconds())+1))
		writeError(w, http.StatusTooManyRequests, "too many attempts: wait a few minutes")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	user, err := s.Auth.Authenticate(ctx, req.Username, req.Password)
	if err != nil {
		name := req.Username
		if len(name) > 64 {
			name = name[:64]
		}
		switch {
		case errors.Is(err, auth.ErrNotAllowed):
			_ = s.Store.Audit(ctx, name, "login.denied", "not in a group that may use the application", false)
			s.byIP.Fail(ipKey)
			time.Sleep(s.FailureDelay)
			writeError(w, http.StatusForbidden, auth.ErrNotAllowed.Error())
		case errors.Is(err, auth.ErrUnavailable):
			s.logf("login: " + err.Error())
			writeError(w, http.StatusServiceUnavailable, "the login service is not available: try again later or ask an administrator")
		default:
			_ = s.Store.Audit(ctx, name, "login.failed", "wrong user name or password", false)
			s.byIP.Fail(ipKey)
			s.byUser.Fail(userKey)
			time.Sleep(s.FailureDelay)
			writeError(w, http.StatusUnauthorized, auth.ErrBadCredentials.Error())
		}
		return
	}
	s.byUser.Reset(userKey)
	token, sess := s.Sessions.Create(user, ip)
	_ = s.Store.Audit(ctx, user.Name, "login", user.Source+", role "+string(user.Role), true)
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: token, Path: "/", HttpOnly: true, Secure: s.Secure, SameSite: http.SameSiteStrictMode,
		MaxAge: int(s.Sessions.Max.Seconds())})
	writeJSON(w, 200, s.sessionInfo(sess))
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
	if cookie, err := r.Cookie(cookieName); err == nil {
		s.Sessions.Delete(cookie.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", HttpOnly: true, Secure: s.Secure, SameSite: http.SameSiteStrictMode, MaxAge: -1})
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}

// pendingKeys remembers for a few minutes which key a host offered when an administrator looked at it, so that "confirm" can only
// confirm exactly that key.
type pendingKeys struct {
	mu sync.Mutex
	m  map[int64]pendingKey
}

type pendingKey struct {
	line    string
	finger  string
	expires time.Time
}

func newPendingKeys() *pendingKeys { return &pendingKeys{m: map[int64]pendingKey{}} }
