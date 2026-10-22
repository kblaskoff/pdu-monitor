package web

import (
	"context"
	"net/http"

	"github.com/kblaskoff/pdu-monitor/server/internal/auth"
)

// What a person may choose for themselves. Everything is a choice of a few words, so that nothing from the browser is kept as it is.
var prefChoices = map[string][]string{
	// cpuIdleMode: how CPU idle is shown. "raw" is what the host reports. "threads" counts the first half of the idle as the bonus of the
	// hyper-threads: 50 % reported is 0 % (no room is left in practice) and 100 % is 100 %, so 90 % reported is 80 %.
	"cpuIdleMode": {"raw", "threads"},
}

func prefDefault(key string) string { return prefChoices[key][0] }

// prefsOf is the choices of the person, with the default where nothing was chosen.
func (s *Server) prefsOf(sess *auth.Session) map[string]string {
	out := map[string]string{}
	for k := range prefChoices {
		out[k] = prefDefault(k)
	}
	if saved, err := s.Store.UserPrefs(context.Background(), auth.UserKey(sess.User)); err == nil {
		for k, v := range saved {
			for _, ok := range prefChoices[k] {
				if v == ok {
					out[k] = v
				}
			}
		}
	}
	return out
}

// handlePrefsPut keeps the choices of the person who asks (never of another one).
func (s *Server) handlePrefsPut(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
	var req map[string]string
	if !readJSON(w, r, &req) {
		return
	}
	for k, v := range req {
		valid := false
		for _, ok := range prefChoices[k] {
			valid = valid || v == ok
		}
		if !valid {
			writeError(w, http.StatusBadRequest, "this choice does not exist: "+k)
			return
		}
	}
	for k, v := range req {
		if err := s.Store.SetUserPref(r.Context(), auth.UserKey(sess.User), k, v); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, s.prefsOf(sess))
}
