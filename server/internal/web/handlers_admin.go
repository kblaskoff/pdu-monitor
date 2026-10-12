package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/kblaskoff/pdu-monitor/server/internal/auth"
	"github.com/kblaskoff/pdu-monitor/server/internal/store"
)

func (s *Server) handleAdminConfig(w http.ResponseWriter, r *http.Request, _ *auth.Session) {
	ctx := r.Context()
	racks, err := s.Store.Racks(ctx)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	pdus, _ := s.Store.PDUs(ctx)
	hosts, _ := s.Store.Hosts(ctx)
	settings, _ := s.Store.Settings(ctx)
	writeJSON(w, 200, map[string]any{"racks": racks, "pdus": nonNil(pdus), "hosts": nonNil(hosts), "settings": settings, "serverKey": s.ServerKey, "demo": s.Demo})
}

func nonNil[T any](v []T) []T {
	if v == nil {
		return []T{}
	}
	return v
}

func (s *Server) saveError(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, 404, "not found")
		return
	}
	writeError(w, 400, err.Error())
}

func (s *Server) handleRackSave(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
	var rack store.Rack
	if !readJSON(w, r, &rack) {
		return
	}
	rack.ID = 0
	if r.Method == http.MethodPut {
		id, ok := pathID(r)
		if !ok {
			writeError(w, 404, "no such rack")
			return
		}
		rack.ID = id
	}
	saved, err := s.Store.SaveRack(r.Context(), rack)
	if err != nil {
		s.saveError(w, err)
		return
	}
	_ = s.Store.Audit(r.Context(), sess.User.Name, "rack.save", fmt.Sprintf("%s (limit %.1f A)", saved.Name, saved.MaxAmps), true)
	writeJSON(w, 200, saved)
}

func (s *Server) handleRackDelete(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, 404, "no such rack")
		return
	}
	if err := s.Store.DeleteRack(r.Context(), id); err != nil {
		s.saveError(w, err)
		return
	}
	_ = s.Store.Audit(r.Context(), sess.User.Name, "rack.delete", strconv.FormatInt(id, 10), true)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

type pduSaveRequest struct {
	store.PDU
	ReadCommunity  string `json:"readCommunity"`
	WriteCommunity string `json:"writeCommunity"`
	ClearWrite     bool   `json:"clearWrite"`
}

func (s *Server) handlePDUSave(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
	var req pduSaveRequest
	if !readJSON(w, r, &req) {
		return
	}
	req.PDU.ID = 0
	if r.Method == http.MethodPut {
		id, ok := pathID(r)
		if !ok {
			writeError(w, 404, "no such PDU")
			return
		}
		req.PDU.ID = id
	}
	saved, err := s.Store.SavePDU(r.Context(), req.PDU, store.Communities{Read: req.ReadCommunity, Write: req.WriteCommunity}, req.ClearWrite)
	if err != nil {
		s.saveError(w, err)
		return
	}
	// the communities are never written to the log
	_ = s.Store.Audit(r.Context(), sess.User.Name, "pdu.save", fmt.Sprintf("%s %s:%d (limit %.1f A)", saved.Name, saved.Host, saved.Port, saved.MaxAmps), true)
	go s.App.PollPDUs(context.Background(), saved.ID)
	writeJSON(w, 200, saved)
}

func (s *Server) handlePDUDelete(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, 404, "no such PDU")
		return
	}
	if err := s.Store.DeletePDU(r.Context(), id); err != nil {
		s.saveError(w, err)
		return
	}
	_ = s.Store.Audit(r.Context(), sess.User.Name, "pdu.delete", strconv.FormatInt(id, 10), true)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) handlePDUTest(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
	var req pduSaveRequest
	if !readJSON(w, r, &req) {
		return
	}
	c := store.Communities{Read: req.ReadCommunity, Write: req.WriteCommunity}
	if req.PDU.ID != 0 && c.Read == "" { // an edit where the community is left as it is: the stored one is used
		stored, err := s.Store.PDUCommunities(r.Context(), req.PDU.ID)
		if err == nil {
			c.Read = stored.Read
		}
	}
	if !req.Vendor.Valid() || req.Host == "" || req.Port <= 0 || req.Port > 65535 {
		writeError(w, 400, "choose the vendor and enter the address and the port")
		return
	}
	snap, err := s.App.TestPDU(r.Context(), req.PDU, c)
	if err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "model": snap.Info.Model, "outlets": snap.Info.Outlets, "amps": snap.TotalAmps, "metered": snap.HasOutletMetering()})
}

func (s *Server) handleHostSave(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
	var h store.Host
	if !readJSON(w, r, &h) {
		return
	}
	h.ID = 0
	if r.Method == http.MethodPut {
		id, ok := pathID(r)
		if !ok {
			writeError(w, 404, "no such host")
			return
		}
		h.ID = id
	}
	if h.Port == 0 {
		h.Port = 22
	}
	saved, err := s.Store.SaveHost(r.Context(), h)
	if err != nil {
		s.saveError(w, err)
		return
	}
	_ = s.Store.Audit(r.Context(), sess.User.Name, "host.save", fmt.Sprintf("%s %s@%s:%d", saved.Name, saved.User, saved.Address, saved.Port), true)
	if s.App.Hosts != nil && saved.HostKey != "" && saved.Enabled {
		go s.App.Hosts.PollOne(context.Background(), saved)
	}
	writeJSON(w, 200, saved)
}

func (s *Server) handleHostDelete(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, 404, "no such host")
		return
	}
	if err := s.Store.DeleteHost(r.Context(), id); err != nil {
		s.saveError(w, err)
		return
	}
	_ = s.Store.Audit(r.Context(), sess.User.Name, "host.delete", strconv.FormatInt(id, 10), true)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// handleHostFetchKey looks at the key the host offers. Nothing is trusted yet: the administrator must compare the fingerprint with the
// one on the host and then confirm it.
func (s *Server) handleHostFetchKey(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
	h, ok := s.findHost(w, r)
	if !ok {
		return
	}
	if s.Keys == nil {
		writeError(w, 503, "not available")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	key, err := s.Keys.FetchHostKey(ctx, h.Address, h.Port)
	if err != nil {
		writeError(w, 502, "could not connect to the host: "+err.Error())
		return
	}
	line := string(ssh.MarshalAuthorizedKey(key))
	finger := ssh.FingerprintSHA256(key)
	s.pendingKeys.mu.Lock()
	s.pendingKeys.m[h.ID] = pendingKey{line: line, finger: finger, expires: s.Now().Add(10 * time.Minute)}
	s.pendingKeys.mu.Unlock()
	writeJSON(w, 200, map[string]any{"fingerprint": finger, "type": key.Type(), "current": h.HostKey != "",
		"howToCheck": "On the host: ssh-keygen -lf /etc/ssh/ssh_host_" + keyFileType(key.Type()) + "_key.pub"})
}

func keyFileType(t string) string {
	switch t {
	case "ssh-ed25519":
		return "ed25519"
	case "ssh-rsa":
		return "rsa"
	}
	return "ecdsa"
}

func (s *Server) handleHostTrust(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
	h, ok := s.findHost(w, r)
	if !ok {
		return
	}
	var req struct {
		Fingerprint string `json:"fingerprint"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	s.pendingKeys.mu.Lock()
	pending, found := s.pendingKeys.m[h.ID]
	s.pendingKeys.mu.Unlock()
	if !found || s.Now().After(pending.expires) || pending.finger != req.Fingerprint || req.Fingerprint == "" {
		writeError(w, 409, "look at the key of the host again, then confirm exactly that fingerprint")
		return
	}
	if err := s.Store.TrustHostKey(r.Context(), h.ID, pending.line); err != nil {
		s.saveError(w, err)
		return
	}
	s.pendingKeys.mu.Lock()
	delete(s.pendingKeys.m, h.ID)
	s.pendingKeys.mu.Unlock()
	_ = s.Store.Audit(r.Context(), sess.User.Name, "host.trust", h.Name+" "+pending.finger, true)
	if s.App.Hosts != nil {
		h.HostKey = pending.line
		go s.App.Hosts.PollOne(context.Background(), h)
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) handleSettingsGet(w http.ResponseWriter, r *http.Request, _ *auth.Session) {
	st, err := s.Store.Settings(r.Context())
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, st)
}

func (s *Server) handleSettingsPut(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
	var st store.Settings
	if !readJSON(w, r, &st) {
		return
	}
	saved, err := s.Store.SaveSettings(r.Context(), st)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	_ = s.Store.Audit(r.Context(), sess.User.Name, "settings", fmt.Sprintf("%+v", saved), true)
	writeJSON(w, 200, saved)
}

// handleImport takes the settings file of the Mac application.
func (s *Server) handleImport(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<20)
	data := make([]byte, 0, 1<<16)
	buf := make([]byte, 32<<10)
	for {
		n, err := r.Body.Read(buf)
		data = append(data, buf[:n]...)
		if err != nil {
			break
		}
	}
	sum, err := s.Store.ImportMacBackup(r.Context(), data)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	_ = s.Store.Audit(r.Context(), sess.User.Name, "import", fmt.Sprintf("%d racks, %d PDUs, %d names", sum.Racks, sum.PDUs, sum.Labels), true)
	go s.App.PollPDUs(context.Background())
	writeJSON(w, 200, sum)
}

func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request, _ *auth.Session) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	log, err := s.Store.AuditLog(r.Context(), limit)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"entries": nonNil(log)})
}
