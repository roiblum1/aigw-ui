package api

import (
	"errors"
	"net/http"

	"aigw-ui/internal/selftest"
)

// startSelfTest begins a self-test of a cluster and returns at once; the
// caller polls getSelfTest for the steps. The body may name the model to
// test with as {"model_id": "..."}.
func (s *Server) startSelfTest(w http.ResponseWriter, r *http.Request) {
	var b struct {
		ModelID string `json:"model_id"`
	}
	// An empty body is fine: the first suitable model is used.
	if r.ContentLength != 0 {
		if err := decode(r, &b); err != nil {
			fail(w, err)
			return
		}
	}
	run, err := s.selftest.Start(r.Context(), r.PathValue("id"), b.ModelID)
	switch {
	case errors.Is(err, selftest.ErrRunning):
		writeError(w, http.StatusConflict, err.Error())
		return
	case errors.Is(err, selftest.ErrNoGatewayURL), errors.Is(err, selftest.ErrNoModel):
		fail(w, invalid("%v", err))
		return
	case err != nil:
		fail(w, err)
		return
	}
	note(r, "selftest", "Started a self-test of cluster "+run.ClusterName+" with model "+run.ModelName)
	writeJSON(w, http.StatusAccepted, run)
}

// getSelfTest returns the last self-test of a cluster, running or finished.
// Runs are kept in memory, so there is none after a restart.
func (s *Server) getSelfTest(w http.ResponseWriter, r *http.Request) {
	run, ok := s.selftest.Get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "no self-test has run for this cluster since the server started")
		return
	}
	writeJSON(w, http.StatusOK, run)
}
