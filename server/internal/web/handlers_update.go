package web

import (
	"fmt"
	"net/http"
	"time"

	"github.com/kblaskoff/pdu-monitor/server/internal/auth"
	"github.com/kblaskoff/pdu-monitor/server/internal/updater"
)

// how long a collector is asked to update: a collector that has not got the news by then (down, or no updater installed) is left alone
const updateWanted = 15 * time.Minute

type updateWish struct {
	ID      string
	At      time.Time
	Version string // what the collector ran when it was asked: a different version later means it did update
}

type collectorUpdate struct {
	Name      string     `json:"name"`
	Version   string     `json:"version"`
	Seen      *time.Time `json:"lastSeen"`
	Requested bool       `json:"requested"`
}

// handleUpdate is what the Software tab shows: this server and the collectors.
func (s *Server) handleUpdate(w http.ResponseWriter, r *http.Request, _ *auth.Session) {
	srv := updater.Status{}
	if s.UpdateDir != "" {
		srv = updater.Read(s.UpdateDir)
	}
	s.collected.mu.Lock()
	now := s.Now()
	cols := make([]collectorUpdate, 0, len(s.Collectors))
	for _, c := range s.Collectors {
		cu := collectorUpdate{Name: c.Name}
		if v, ok := s.collected.seen[c.Name]; ok {
			at := v.At
			cu.Seen, cu.Version = &at, v.Version
		}
		if wish, ok := s.collected.wanted[c.Name]; ok && now.Sub(wish.At) < updateWanted {
			cu.Requested = true
		}
		cols = append(cols, cu)
	}
	s.collected.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"version": s.Version, "server": srv, "collectors": cols})
}

type updateRequest struct {
	Confirm   bool   `json:"confirm"`
	Collector string `json:"collector"` // one collector; empty is all of them
}

// handleUpdateServer asks the root side of this machine to update the server. The answer comes at once: the update restarts this very
// process, so the screen follows update.status afterwards.
func (s *Server) handleUpdateServer(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
	var req updateRequest
	if !readJSON(w, r, &req) {
		return
	}
	if !req.Confirm {
		writeError(w, http.StatusBadRequest, "the update must be confirmed")
		return
	}
	if s.UpdateDir == "" {
		writeError(w, http.StatusConflict, "this server does not know where its updater is")
		return
	}
	if err := updater.Request(s.UpdateDir, sess.User.Name); err != nil {
		code := http.StatusInternalServerError
		if err == updater.ErrNotInstalled || err == updater.ErrPending {
			code = http.StatusConflict
		}
		writeError(w, code, err.Error())
		return
	}
	_ = s.Store.Audit(r.Context(), sess.User.Name, "update.server", s.Version, true)
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true})
}

// handleUpdateCollectors tells collectors (all, or the one named) to update: they hear it in the answer to their next report, so it takes
// up to a minute, and nothing here connects to them.
func (s *Server) handleUpdateCollectors(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
	var req updateRequest
	if !readJSON(w, r, &req) {
		return
	}
	if !req.Confirm {
		writeError(w, http.StatusBadRequest, "the update must be confirmed")
		return
	}
	if req.Collector != "" && !s.knownCollector(req.Collector) {
		writeError(w, http.StatusNotFound, "no such collector")
		return
	}
	now := s.Now()
	s.collected.mu.Lock()
	var names []string
	for _, c := range s.Collectors {
		if req.Collector != "" && c.Name != req.Collector {
			continue
		}
		s.collected.wanted[c.Name] = updateWish{ID: fmt.Sprint(now.UnixNano()), At: now, Version: s.collected.seen[c.Name].Version}
		names = append(names, c.Name)
	}
	s.collected.mu.Unlock()
	if len(names) == 0 {
		writeError(w, http.StatusConflict, "no collector is configured on this server")
		return
	}
	scope := "all"
	if req.Collector != "" {
		scope = req.Collector
	}
	_ = s.Store.Audit(r.Context(), sess.User.Name, "update.collectors", scope, true)
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "collectors": names})
}

// updateFor is called with the lock held when a collector reports: it returns the id to send back, and forgets the wish once the
// collector has taken it (seen), has a different version than when it was asked, or has had it too long.
func (c *collectors) updateFor(name, seen, version string, now time.Time) string {
	wish, ok := c.wanted[name]
	if !ok {
		return ""
	}
	if wish.ID == seen || now.Sub(wish.At) >= updateWanted || (version != "" && wish.Version != "" && version != wish.Version) {
		delete(c.wanted, name)
		return ""
	}
	return wish.ID
}
