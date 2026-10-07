package api

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"aigw-ui/internal/usage"
)

type usageResponse struct {
	// Enabled is false when the server has no Redis to read from.
	Enabled bool `json:"enabled"`
	// CanReset reports whether the server is allowed to reset a quota's usage.
	CanReset bool `json:"can_reset"`
	usage.Report
}

// getUsage reports the tokens used in the current window. ?tenant_id= limits
// it to one tenant.
func (s *Server) getUsage(w http.ResponseWriter, r *http.Request) {
	if s.usage == nil {
		writeJSON(w, http.StatusOK, usageResponse{Report: usage.Report{At: time.Now().UTC(), Quotas: []usage.Quota{}, Pools: []usage.Pool{}}})
		return
	}
	report, err := s.usage.Report(r.Context(), r.URL.Query().Get("tenant_id"))
	if err != nil {
		failUsage(w, err)
		return
	}
	writeJSON(w, http.StatusOK, usageResponse{Enabled: true, CanReset: s.usage.CanReset(), Report: report})
}

// resetUsage sets the usage of one tenant on one model back to zero for the
// current window.
func (s *Server) resetUsage(w http.ResponseWriter, r *http.Request) {
	if s.usage == nil {
		writeError(w, http.StatusConflict, "usage monitoring is off: the server has no Redis configured")
		return
	}
	res, err := s.usage.Reset(r.Context(), r.PathValue("id"), r.PathValue("model_id"))
	summary := "Reset usage of " + res.TenantSlug + " on " + res.ModelName
	var redisErr usage.RedisError
	switch {
	case errors.As(err, &redisErr):
		// The tenant and model were found, so the attempt belongs in the log.
		s.log(r, "usage.reset", summary, false, "Redis: "+err.Error())
		failUsage(w, err)
		return
	case err != nil:
		failUsage(w, err)
		return
	}
	s.log(r, "usage.reset", summary, true, fmt.Sprintf("%d of %d counters held usage and were cleared.", res.Deleted, res.Counters))
	slog.Info("usage reset", "tenant", res.TenantSlug, "model", res.ModelName, "counters", res.Counters, "deleted", res.Deleted)
	writeJSON(w, http.StatusOK, res)
}

// failUsage maps the errors of the usage service to HTTP statuses.
func failUsage(w http.ResponseWriter, err error) {
	var redisErr usage.RedisError
	switch {
	case errors.As(err, &redisErr):
		writeError(w, http.StatusBadGateway, "Redis: "+err.Error())
	case errors.Is(err, usage.ErrResetDisabled):
		writeError(w, http.StatusForbidden, "resetting usage is turned off; set redis.allowReset (REDIS_ALLOW_RESET=true) to allow it")
	case errors.Is(err, usage.ErrNoQuota):
		writeError(w, http.StatusNotFound, "this tenant has no quota on that model that is applied to a cluster")
	default:
		fail(w, err)
	}
}
