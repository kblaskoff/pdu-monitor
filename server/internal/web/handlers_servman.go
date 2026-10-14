package web

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/kblaskoff/pdu-monitor/server/internal/app"
	"github.com/kblaskoff/pdu-monitor/server/internal/auth"
)

func (s *Server) servmanError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, app.ErrNoServman):
		writeError(w, http.StatusNotFound, "servman is not configured on this server (the servman section of config.yaml)")
	case errors.Is(err, app.ErrNamesBusy):
		writeError(w, http.StatusConflict, err.Error())
	default:
		writeError(w, http.StatusBadGateway, err.Error())
	}
}

// handleServman is the last plan and the state of the writing.
func (s *Server) handleServman(w http.ResponseWriter, r *http.Request, _ *auth.Session) {
	writeJSON(w, http.StatusOK, s.App.ServmanStatus())
}

// handleServmanRefresh asks servman now and compares it with the PDUs. Nothing is written.
func (s *Server) handleServmanRefresh(w http.ResponseWriter, r *http.Request, _ *auth.Session) {
	if _, err := s.App.ServmanPlan(r.Context()); err != nil {
		s.servmanError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.App.ServmanStatus())
}

type servmanApply struct {
	All     bool          `json:"all"`
	PDU     string        `json:"pdu"`
	Outlet  int           `json:"outlet"`
	Outlets []app.NameRef `json:"outlets"` // a list of outlets (to try the failed ones again)
	Confirm bool          `json:"confirm"`
}

// handleServmanApply starts the writing of the names (it goes on in the background; the screen follows the status). It needs the same
// confirmation as switching power does: a request without "confirm" writes nothing.
func (s *Server) handleServmanApply(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
	var req servmanApply
	if !readJSON(w, r, &req) {
		return
	}
	if !req.Confirm {
		writeError(w, http.StatusBadRequest, "the writing must be confirmed")
		return
	}
	if req.Outlet < 0 || req.Outlet > 96 || len(req.PDU) > 100 || len(req.Outlets) > 2000 {
		writeError(w, http.StatusBadRequest, "bad choice")
		return
	}
	for _, o := range req.Outlets {
		if o.Outlet < 1 || o.Outlet > 96 || o.PDU == "" || len(o.PDU) > 100 {
			writeError(w, http.StatusBadRequest, "bad choice")
			return
		}
	}
	sel := app.NameSelection{All: req.All, PDU: req.PDU, Outlet: req.Outlet, Only: req.Outlets}
	if err := s.App.StartApplyNames(sess.User.Name, sel); err != nil {
		if errors.Is(err, app.ErrNoServman) || errors.Is(err, app.ErrNamesBusy) {
			s.servmanError(w, err)
		} else {
			writeError(w, http.StatusBadRequest, err.Error())
		}
		return
	}
	scope := "everything in the plan"
	if !req.All {
		switch {
		case len(req.Outlets) > 0:
			scope = fmt.Sprintf("%d outlets again", len(req.Outlets))
		case req.Outlet != 0:
			scope = fmt.Sprintf("%s outlet %d", req.PDU, req.Outlet)
		default:
			scope = req.PDU
		}
	}
	_ = s.Store.Audit(r.Context(), sess.User.Name, "servman.apply", scope, true)
	writeJSON(w, http.StatusAccepted, s.App.ServmanStatus())
}

// servmanBrief is what every user sees of servman on the overview: the conflicts (mistakes in servman: two devices on one outlet),
// which are not supposed to exist, and when servman was last compared.
func (s *Server) servmanBrief() map[string]any {
	st := s.App.ServmanStatus()
	out := map[string]any{"configured": st.Configured, "conflicts": []any{}}
	if !st.Configured {
		return out
	}
	if st.Plan != nil {
		out["conflicts"] = st.Plan.Conflicts
		out["checkedAt"] = st.PlanAt
	}
	if st.PlanError != "" {
		out["error"] = st.PlanError
	}
	return out
}
