package web

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kblaskoff/pdu-monitor/server/internal/app"
	"github.com/kblaskoff/pdu-monitor/server/internal/auth"
	"github.com/kblaskoff/pdu-monitor/server/internal/hostinfo"
	"github.com/kblaskoff/pdu-monitor/server/internal/pdu"
	"github.com/kblaskoff/pdu-monitor/server/internal/store"
)

type pduBrief struct {
	ID      int64      `json:"id"`
	RackID  int64      `json:"rackId"`
	Name    string     `json:"name"`
	Vendor  pdu.Vendor `json:"vendor"`
	Model   string     `json:"model,omitempty"`
	Amps    *float64   `json:"amps"`
	Watts   *float64   `json:"watts"`
	MaxAmps float64    `json:"maxAmps"`
	Status  pdu.Status `json:"status"`
	Error   string     `json:"error,omitempty"`
}

type rackBrief struct {
	ID      int64       `json:"id"`
	Name    string      `json:"name"`
	Limit   float64     `json:"limit"`
	Summary pdu.Summary `json:"summary"`
	PDUs    []pduBrief  `json:"pdus"`
}

// pduData is everything the pages need about the PDUs, read once per request.
type pduData struct {
	states  []pdu.State
	configs map[int64]store.PDU
	byRack  map[int64][]pdu.State
	warn    float64
	racks   []store.Rack
}

func (s *Server) loadPDUs(ctx context.Context) (*pduData, error) {
	states, configs, err := s.App.States(ctx)
	if err != nil {
		return nil, err
	}
	racks, err := s.Store.Racks(ctx)
	if err != nil {
		return nil, err
	}
	st, _ := s.Store.Settings(ctx)
	d := &pduData{states: states, configs: configs, byRack: map[int64][]pdu.State{}, warn: float64(st.WarnPercent) / 100, racks: racks}
	for _, p := range states {
		d.byRack[configs[p.ID].RackID] = append(d.byRack[configs[p.ID].RackID], p)
	}
	return d, nil
}

func briefOf(p pdu.State, rack int64) pduBrief {
	b := pduBrief{ID: p.ID, RackID: rack, Name: p.Name, Vendor: p.Vendor, Amps: p.Amps(), Watts: p.Watts(), MaxAmps: p.MaxAmps, Error: p.Error}
	if p.Snapshot != nil {
		b.Model = p.Snapshot.Info.Model
	}
	return b
}

func (d *pduData) rackBrief(r store.Rack) rackBrief {
	pdus := d.byRack[r.ID]
	out := rackBrief{ID: r.ID, Name: r.Name, Limit: r.MaxAmps, Summary: pdu.Summarize(r.MaxAmps, pdus, d.warn), PDUs: []pduBrief{}}
	for _, p := range pdus {
		b := briefOf(p, r.ID)
		b.Status = p.Status(d.warn)
		out.PDUs = append(out.PDUs, b)
	}
	return out
}

// ---- hosts

type hostBrief struct {
	ID         int64                 `json:"id"`
	Name       string                `json:"name"`
	Address    string                `json:"address"`
	Port       int                   `json:"port"`
	RackID     *int64                `json:"rackId"`
	RackName   string                `json:"rackName,omitempty"`
	OutletKey  string                `json:"outletKey"`
	Enabled    bool                  `json:"enabled"`
	Trusted    bool                  `json:"trusted"`
	Collector  string                `json:"collector,omitempty"`
	Online     bool                  `json:"online"`
	Stale      bool                  `json:"stale"`
	AgeSeconds *int64                `json:"ageSeconds"`
	Error      string                `json:"error,omitempty"`
	Cores      *int                  `json:"cores"`
	IdlePct    *float64              `json:"idlePct"`
	Load       []float64             `json:"load,omitempty"`
	MemUsed    *int64                `json:"memUsed"`
	MemTotal   *int64                `json:"memTotal"`
	MemArc     *int64                `json:"memArc"`
	VMs        *int                  `json:"vms"`
	Pools      []hostinfo.Pool       `json:"pools,omitempty"`
	LVM        []hostinfo.VG         `json:"lvm,omitempty"`
	FS         []hostinfo.Filesystem `json:"fs,omitempty"`
	Agent      string                `json:"agent,omitempty"`
	Problems   []string              `json:"problems,omitempty"`
	Power      *powerView            `json:"power,omitempty"`
	VMList     []hostinfo.VM         `json:"vmList,omitempty"`
	OtherCores *float64              `json:"otherCores"`
}

// powerView is what the PDUs say about the cables of a host.
type powerView struct {
	Server string   `json:"server"`
	RackID int64    `json:"rackId"`
	Watts  *float64 `json:"watts"`
	Amps   *float64 `json:"amps"`
	State  string   `json:"state"`
	Ports  int      `json:"ports"`
}

func (s *Server) hostBriefs(ctx context.Context, d *pduData, full bool) ([]hostBrief, error) {
	hosts, err := s.Store.Hosts(ctx)
	if err != nil {
		return nil, err
	}
	st, _ := s.Store.Settings(ctx)
	now := s.Now()
	rackNames := map[int64]string{}
	servers := map[int64]map[string]pdu.Server{}
	for _, r := range d.racks {
		rackNames[r.ID] = r.Name
		m := map[string]pdu.Server{}
		for _, sv := range pdu.Servers(d.byRack[r.ID]) {
			m[sv.Key] = sv
		}
		servers[r.ID] = m
	}
	states := map[int64]hostinfo.State{}
	if s.App.Hosts != nil {
		for _, h := range s.App.Hosts.States() {
			states[h.Host.ID] = h
		}
	}
	out := make([]hostBrief, 0, len(hosts))
	for _, h := range hosts {
		b := hostBrief{ID: h.ID, Name: h.Name, Address: h.Address, Port: h.Port, RackID: h.RackID, OutletKey: h.OutletKey, Enabled: h.Enabled, Trusted: h.HostKey != "" || h.Collector != "", Collector: h.Collector}
		if h.RackID != nil {
			b.RackName = rackNames[*h.RackID]
		}
		hs, ok := states[h.ID]
		switch {
		case !h.Enabled:
			b.Error = "disabled"
		case h.HostKey == "" && h.Collector == "":
			b.Error = "the key of this host is not confirmed yet"
		case ok:
			b.Error = hs.Error
		case h.Collector != "":
			b.Error = "waiting for the collector " + h.Collector
		default:
			b.Error = "not read yet"
		}
		if ok && hs.Status != nil {
			stat := hs.Status
			age := int64(now.Sub(time.Unix(stat.T, 0)).Seconds())
			if age < 0 {
				age = 0
			}
			b.AgeSeconds = &age
			b.Stale = age > int64(3*st.HostPollSeconds)+30
			b.Online = hs.Error == "" && !b.Stale
			b.Cores, b.IdlePct, b.Load = stat.CPU.Cores, stat.CPU.IdlePct, stat.CPU.Load
			if stat.Mem != nil {
				b.MemUsed, b.MemTotal, b.MemArc = &stat.Mem.Used, &stat.Mem.Total, stat.Mem.ZfsArc
			}
			n := stat.VMs.Count
			b.VMs = &n
			if stat.Storage != nil {
				b.Pools, b.LVM = stat.Storage.Pools, stat.Storage.LVM
				if full {
					b.FS = stat.Storage.FS // only the page of one host needs the list of filesystems
				}
			}
			b.Agent = stat.Agent.Version
			b.Problems = stat.Errors
			b.OtherCores = stat.OtherCores
			if full {
				b.VMList = stat.VMs.List
			}
		}
		if h.OutletKey != "" {
			key := pdu.ServerKey(h.OutletKey)
			search := []int64{}
			if h.RackID != nil {
				search = append(search, *h.RackID)
			} else {
				for _, r := range d.racks {
					search = append(search, r.ID)
				}
			}
			for _, rid := range search {
				if sv, ok := servers[rid][key]; ok {
					b.Power = &powerView{Server: sv.Name, RackID: rid, Watts: sv.Watts(), Amps: sv.Amps(), State: sv.Power(), Ports: len(sv.Ports)}
					break
				}
			}
		}
		out = append(out, b)
	}
	return out, nil
}

// ---- pages

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request, _ *auth.Session) {
	ctx := r.Context()
	d, err := s.loadPDUs(ctx)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	racks := make([]rackBrief, 0, len(d.racks))
	var amps, watts float64
	haveA, haveW := false, false
	over := 0
	for _, r := range d.racks {
		b := d.rackBrief(r)
		racks = append(racks, b)
		if b.Summary.Amps != nil {
			amps += *b.Summary.Amps
			haveA = true
		}
		if b.Summary.Watts != nil {
			watts += *b.Summary.Watts
			haveW = true
		}
		if b.Summary.Attention == pdu.StatusOver {
			over++
		}
	}
	totals := map[string]any{"racks": len(racks), "over": over, "amps": nil, "watts": nil}
	if haveA {
		totals["amps"] = amps
	}
	if haveW {
		totals["watts"] = watts
	}
	hosts, _ := s.hostBriefs(ctx, d, false)
	hs := map[string]int{"total": len(hosts)}
	for _, h := range hosts {
		switch {
		case !h.Enabled:
			hs["disabled"]++
		case !h.Trusted:
			hs["untrusted"]++
		case h.Online:
			hs["online"]++
		case h.Stale:
			hs["stale"]++
		default:
			hs["offline"]++
		}
		for _, p := range h.Pools {
			if p.Health != "ONLINE" {
				hs["degraded"]++
				break
			}
		}
	}
	writeJSON(w, 200, map[string]any{"now": s.Now().Unix(), "totals": totals, "racks": racks, "hosts": hs})
}

type portView struct {
	PDU     int64    `json:"pdu"`
	PDUName string   `json:"pduName"`
	Outlet  int      `json:"outlet"`
	On      *bool    `json:"on"`
	Amps    *float64 `json:"amps"`
	Watts   *float64 `json:"watts"`
}

type hostRef struct {
	ID         int64    `json:"id"`
	Name       string   `json:"name"`
	Online     bool     `json:"online"`
	IdlePct    *float64 `json:"idlePct"`
	VMs        *int     `json:"vms"`
	AgeSeconds *int64   `json:"ageSeconds"`
}

type serverView struct {
	Key   string     `json:"key"`
	Name  string     `json:"name"`
	Kind  string     `json:"kind"`
	Label string     `json:"label,omitempty"`
	Power string     `json:"power"`
	Watts *float64   `json:"watts"`
	Amps  *float64   `json:"amps"`
	Ports []portView `json:"ports"`
	Host  *hostRef   `json:"host,omitempty"`
}

func (s *Server) handleRack(w http.ResponseWriter, r *http.Request, _ *auth.Session) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, 404, "no such rack")
		return
	}
	ctx := r.Context()
	d, err := s.loadPDUs(ctx)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	var rack *store.Rack
	for i := range d.racks {
		if d.racks[i].ID == id {
			rack = &d.racks[i]
		}
	}
	if rack == nil {
		writeError(w, 404, "no such rack")
		return
	}
	labels, _ := s.Store.Labels(ctx)
	hosts, _ := s.hostBriefs(ctx, d, false)
	hostByKey := map[string]hostBrief{}
	for _, h := range hosts {
		if h.OutletKey != "" && (h.RackID == nil || *h.RackID == id) {
			hostByKey[pdu.ServerKey(h.OutletKey)] = h
		}
	}
	servers := []serverView{}
	for _, sv := range pdu.Servers(d.byRack[id]) {
		v := serverView{Key: sv.Key, Name: sv.Name, Kind: sv.Kind, Label: labels[id][sv.Key], Power: sv.Power(), Watts: sv.Watts(), Amps: sv.Amps(), Ports: []portView{}}
		for _, p := range sv.Ports {
			v.Ports = append(v.Ports, portView{PDU: p.PDUID, PDUName: p.PDUName, Outlet: p.Outlet, On: p.Reading.On, Amps: p.Reading.Amps, Watts: p.Reading.Watts})
		}
		if h, ok := hostByKey[sv.Key]; ok {
			v.Host = &hostRef{ID: h.ID, Name: h.Name, Online: h.Online, IdlePct: h.IdlePct, VMs: h.VMs, AgeSeconds: h.AgeSeconds}
		}
		servers = append(servers, v)
	}
	writeJSON(w, 200, map[string]any{"rack": d.rackBrief(*rack), "servers": servers})
}

func (s *Server) handleLabel(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, 404, "no such rack")
		return
	}
	var req struct{ Key, Label string }
	if !readJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Key) == "" {
		writeError(w, 400, "which server?")
		return
	}
	if err := s.Store.SetLabel(r.Context(), id, req.Key, req.Label); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	_ = s.Store.Audit(r.Context(), sess.User.Name, "label", req.Key+" = "+req.Label, true)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) handlePDU(w http.ResponseWriter, r *http.Request, _ *auth.Session) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, 404, "no such PDU")
		return
	}
	d, err := s.loadPDUs(r.Context())
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	for _, p := range d.states {
		if p.ID == id {
			cfg := d.configs[id]
			rackName := ""
			for _, rk := range d.racks {
				if rk.ID == cfg.RackID {
					rackName = rk.Name
				}
			}
			b := briefOf(p, cfg.RackID)
			b.Status = p.Status(d.warn)
			writeJSON(w, 200, map[string]any{"pdu": b, "rackName": rackName, "bankMaxAmps": cfg.BankMaxAmps, "host": cfg.Host, "port": cfg.Port,
				"snapshot": p.Snapshot, "hasOutletMetering": p.Snapshot != nil && p.Snapshot.HasOutletMetering()})
			return
		}
	}
	writeError(w, 404, "no such PDU")
}

func (s *Server) handlePDUHistory(w http.ResponseWriter, r *http.Request, _ *auth.Session) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, 404, "no such PDU")
		return
	}
	hours, _ := strconv.Atoi(r.URL.Query().Get("hours"))
	if hours < 1 || hours > 24*30 {
		hours = 24
	}
	samples, err := s.Store.Samples(r.Context(), id, s.Now().Add(-time.Duration(hours)*time.Hour))
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	type point struct {
		T     int64    `json:"t"`
		Amps  *float64 `json:"amps"`
		Watts *float64 `json:"watts"`
	}
	// at most about 240 points: the samples of a bucket are averaged
	bucket := time.Duration(hours) * time.Hour / 240
	if bucket < time.Minute {
		bucket = time.Minute
	}
	out := []point{}
	var sumA, sumW float64
	var nA, nW int
	var start time.Time
	flush := func() {
		if nA == 0 && nW == 0 {
			return
		}
		p := point{T: start.Unix()}
		if nA > 0 {
			a := sumA / float64(nA)
			p.Amps = &a
		}
		if nW > 0 {
			wv := sumW / float64(nW)
			p.Watts = &wv
		}
		out = append(out, p)
		sumA, sumW, nA, nW = 0, 0, 0, 0
	}
	for _, sm := range samples {
		if start.IsZero() || sm.Time.Sub(start) >= bucket {
			flush()
			start = sm.Time
		}
		if sm.Amps != nil {
			sumA += *sm.Amps
			nA++
		}
		if sm.Watts != nil {
			sumW += *sm.Watts
			nW++
		}
	}
	flush()
	writeJSON(w, 200, map[string]any{"hours": hours, "points": out})
}

func (s *Server) handleHosts(w http.ResponseWriter, r *http.Request, _ *auth.Session) {
	d, err := s.loadPDUs(r.Context())
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	hosts, err := s.hostBriefs(r.Context(), d, false)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	sort.SliceStable(hosts, func(i, j int) bool { return hosts[i].Name < hosts[j].Name })
	writeJSON(w, 200, map[string]any{"hosts": hosts, "windows": hostinfo.Windows})
}

func (s *Server) findHost(w http.ResponseWriter, r *http.Request) (store.Host, bool) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, 404, "no such host")
		return store.Host{}, false
	}
	h, err := s.Store.Host(r.Context(), id)
	if err != nil {
		writeError(w, 404, "no such host")
		return h, false
	}
	return h, true
}

func (s *Server) handleHost(w http.ResponseWriter, r *http.Request, _ *auth.Session) {
	h, ok := s.findHost(w, r)
	if !ok {
		return
	}
	d, err := s.loadPDUs(r.Context())
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	hosts, _ := s.hostBriefs(r.Context(), d, true)
	for _, b := range hosts {
		if b.ID == h.ID {
			writeJSON(w, 200, map[string]any{"host": b, "windows": hostinfo.Windows})
			return
		}
	}
	writeError(w, 404, "no such host")
}

func (s *Server) handleTop(w http.ResponseWriter, r *http.Request, _ *auth.Session) {
	h, ok := s.findHost(w, r)
	if !ok {
		return
	}
	window := r.URL.Query().Get("window")
	if window == "" {
		window = "6h"
	}
	if !hostinfo.ValidWindow(window) {
		writeError(w, 400, "the period must be 1h, 6h, 24h or 7d")
		return
	}
	if !h.Enabled || (h.HostKey == "" && h.Collector == "") {
		writeError(w, 409, "this host is not being read (disabled, or its key is not confirmed)")
		return
	}
	if s.App.Hosts == nil {
		writeError(w, 503, "hosts are not available")
		return
	}
	if h.Collector != "" { // the collector pushes these every few minutes; nobody asks the host from here
		top, _, ok := s.App.Hosts.CachedTop(h, window)
		if !ok {
			writeError(w, 409, "the collector "+h.Collector+" has not sent this yet: it does so every few minutes")
			return
		}
		writeJSON(w, 200, top)
		return
	}
	top, err := s.App.Hosts.Top(r.Context(), h, window)
	if err != nil {
		writeError(w, 502, "the host did not answer: "+err.Error())
		return
	}
	writeJSON(w, 200, top)
}

// ---- power

type powerRequest struct {
	Action  string `json:"action"`
	Confirm bool   `json:"confirm"`
	Targets []struct {
		PDU    int64 `json:"pdu"`
		Outlet int   `json:"outlet"`
	} `json:"targets"`
}

func (s *Server) handlePower(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
	var req powerRequest
	if !readJSON(w, r, &req) {
		return
	}
	if !req.Confirm {
		writeError(w, 400, "the action must be confirmed")
		return
	}
	targets := make([]pdu.Target, 0, len(req.Targets))
	for _, t := range req.Targets {
		targets = append(targets, pdu.Target{PDUID: t.PDU, Outlet: t.Outlet})
	}
	op, err := s.App.StartPower(r.Context(), sess.User.Name, req.Action, targets)
	if err != nil {
		status := 400
		if errors.Is(err, app.ErrBusy) {
			status = 409
		}
		writeError(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, op)
}

func (s *Server) handleOps(w http.ResponseWriter, r *http.Request, _ *auth.Session) {
	writeJSON(w, 200, map[string]any{"operations": s.App.RecentOperations()})
}

func (s *Server) handleOp(w http.ResponseWriter, r *http.Request, _ *auth.Session) {
	op, ok := s.App.Operation(r.PathValue("id"))
	if !ok {
		writeError(w, 404, "no such operation")
		return
	}
	writeJSON(w, 200, op)
}
