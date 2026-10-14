package web

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kblaskoff/pdu-monitor/server/internal/hostinfo"
	"github.com/kblaskoff/pdu-monitor/server/internal/store"
)

// CollectorAuth is one collector that may push readings; Hash is the SHA-256 of its token.
type CollectorAuth struct {
	Name string
	Hash [sha256.Size]byte
}

type collectorSeen struct {
	At      time.Time
	Hosts   int
	Version string
}

type collectors struct {
	mu     sync.Mutex
	seen   map[string]collectorSeen
	wanted map[string]updateWish // collectors that are to update, by name
}

// CollectorStatus is what the administrator sees of one collector.
type CollectorStatus struct {
	Name    string     `json:"name"`
	Seen    *time.Time `json:"lastSeen"`
	Hosts   int        `json:"hosts"`
	Version string     `json:"version"`
}

func (s *Server) collectorStatus() []CollectorStatus {
	s.collected.mu.Lock()
	defer s.collected.mu.Unlock()
	out := make([]CollectorStatus, 0, len(s.Collectors))
	for _, c := range s.Collectors {
		st := CollectorStatus{Name: c.Name}
		if v, ok := s.collected.seen[c.Name]; ok {
			at := v.At
			st.Seen, st.Hosts, st.Version = &at, v.Hosts, v.Version
		}
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// the most a report may hold: a collector reads at most a few hundred hosts
const maxCollectedHosts = 500

type collectedHost struct {
	Name   string                     `json:"name"`
	Status json.RawMessage            `json:"status"` // the answer of the agent as it was; null when only the tops are sent or the host did not answer
	Error  string                     `json:"error"`
	Tops   map[string]json.RawMessage `json:"tops"`
}

type collectorReport struct {
	Version    string          `json:"version"`
	UpdateSeen string          `json:"update_seen"` // the id of the update request the collector has acted on
	Hosts      []collectedHost `json:"hosts"`
}

type rejectedHost struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// collectorFor finds the collector that owns the token in the Authorization header (nil if none).
func (s *Server) collectorFor(r *http.Request) *CollectorAuth {
	h := r.Header.Get("Authorization")
	token, ok := strings.CutPrefix(h, "Bearer ")
	if !ok || len(token) < 20 || len(token) > 200 {
		return nil
	}
	sum := sha256.Sum256([]byte(token))
	var found *CollectorAuth
	for i := range s.Collectors { // all are compared, so that the time does not tell which one matched
		if subtle.ConstantTimeCompare(sum[:], s.Collectors[i].Hash[:]) == 1 {
			found = &s.Collectors[i]
		}
	}
	return found
}

// handleCollectorReport takes the readings a collector pushes. There is no session and no cookie here: the token is the login.
func (s *Server) handleCollectorReport(w http.ResponseWriter, r *http.Request) {
	ip := "collector:" + remoteIP(r)
	if ok, retry := s.byIP.Allow(ip); !ok {
		w.Header().Set("Retry-After", fmt.Sprint(int(retry.Seconds())+1))
		writeError(w, http.StatusTooManyRequests, "too many wrong tokens: wait a little")
		return
	}
	col := s.collectorFor(r)
	if col == nil {
		s.byIP.Fail(ip)
		time.Sleep(s.FailureDelay)
		writeError(w, http.StatusUnauthorized, "the token is not known")
		return
	}
	var rep collectorReport
	r.Body = http.MaxBytesReader(w, r.Body, 8<<20)
	if err := json.NewDecoder(r.Body).Decode(&rep); err != nil {
		writeError(w, http.StatusBadRequest, "the report is not valid JSON")
		return
	}
	if len(rep.Hosts) > maxCollectedHosts {
		writeError(w, http.StatusBadRequest, "too many hosts in one report")
		return
	}
	ctx := r.Context()
	var rejected []rejectedHost
	accepted, readings := 0, 0 // readings: hosts with a status or an error (a report with only the busiest VMs does not count)
	for _, ch := range rep.Hosts {
		host, created, err := s.Store.CollectedHost(ctx, col.Name, ch.Name)
		if err != nil {
			reason := err.Error()
			if errors.Is(err, store.ErrOtherCollector) {
				reason = "this name is a host that the server reads itself or that belongs to another collector"
			}
			rejected = append(rejected, rejectedHost{ch.Name, reason})
			continue
		}
		if created {
			_ = s.Store.Audit(ctx, "collector:"+col.Name, "host.discovered", host.Name, true)
		}
		if !host.Enabled { // disabled on the server: nothing is kept
			rejected = append(rejected, rejectedHost{ch.Name, "disabled on the server"})
			continue
		}
		accepted++
		if s.App.Hosts == nil {
			continue
		}
		hasStatus := len(ch.Status) > 0 && string(ch.Status) != "null"
		if hasStatus || ch.Error != "" {
			readings++
		}
		switch {
		case hasStatus:
			status, err := hostinfo.ParseStatus(ch.Status)
			if err != nil {
				s.App.Hosts.Ingest(host, nil, hostinfo.CleanText(err.Error()))
			} else {
				s.App.Hosts.Ingest(host, status, "")
			}
		case ch.Error != "":
			s.App.Hosts.Ingest(host, nil, hostinfo.CleanText(ch.Error))
		}
		for window, raw := range ch.Tops {
			if !hostinfo.ValidWindow(window) {
				continue
			}
			if top, err := hostinfo.ParseTop(raw, window); err == nil {
				s.App.Hosts.IngestTop(host, window, top)
			}
		}
	}
	s.byIP.Reset(ip)
	s.collected.mu.Lock()
	seen := s.collected.seen[col.Name]
	seen.At = s.Now()
	if readings > 0 {
		seen.Hosts = readings
	}
	if v := hostinfo.CleanText(rep.Version); v != "" {
		seen.Version = v
	}
	s.collected.seen[col.Name] = seen
	update := s.collected.updateFor(col.Name, hostinfo.CleanText(rep.UpdateSeen), seen.Version, s.Now())
	s.collected.mu.Unlock()
	writeJSON(w, 200, map[string]any{"ok": true, "accepted": accepted, "rejected": nonNil(rejected), "update": update})
}

func (s *Server) knownCollector(name string) bool {
	for _, c := range s.Collectors {
		if c.Name == name {
			return true
		}
	}
	return false
}
