package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"aigw-ui/internal/render"
	"aigw-ui/internal/selftest"
	"aigw-ui/internal/store"
)

func (s *Server) listTenants(w http.ResponseWriter, r *http.Request) {
	out, err := s.st.ListTenants(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) createTenant(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Slug        string `json:"slug"`
		DisplayName string `json:"display_name"`
	}
	if err := decode(r, &b); err != nil {
		fail(w, err)
		return
	}
	switch {
	case !tenantSlug.MatchString(b.Slug):
		fail(w, invalid("slug must be 1-40 lowercase letters, digits and dashes"))
		return
	case strings.HasPrefix(b.Slug, selftest.TenantPrefix):
		// The self-test deletes leftover tenants by this prefix.
		fail(w, invalid("a slug must not start with %q, which is kept for the self-test", selftest.TenantPrefix))
		return
	}
	t, err := s.st.CreateTenant(r.Context(), b.Slug, b.DisplayName)
	if err != nil {
		fail(w, err)
		return
	}
	s.log(r, "tenant.add", "Added tenant "+t.Slug, true,
		"Saved. Nothing is applied to the clusters until the tenant has a key or a quota.")
	writeJSON(w, http.StatusCreated, t)
}

func (s *Server) getTenant(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	t, err := s.st.GetTenant(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	keys, err := s.st.ListKeys(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	quotas, err := s.st.ListQuotas(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tenant": t, "keys": keys, "quotas": quotas})
}

func (s *Server) updateTenant(w http.ResponseWriter, r *http.Request) {
	var b struct {
		DisplayName string `json:"display_name"`
		Enabled     bool   `json:"enabled"`
	}
	if err := decode(r, &b); err != nil {
		fail(w, err)
		return
	}
	t, err := s.st.UpdateTenant(r.Context(), r.PathValue("id"), b.DisplayName, b.Enabled)
	if err != nil {
		fail(w, err)
		return
	}
	state := "Enabled"
	if !t.Enabled {
		state = "Disabled"
	}
	s.changed(r, "tenant.update", state+" tenant "+t.Slug)
	writeJSON(w, http.StatusOK, t)
}

func (s *Server) deleteTenant(w http.ResponseWriter, r *http.Request) {
	t, err := s.st.GetTenant(r.Context(), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	if err := s.st.DeleteTenant(r.Context(), t.ID); err != nil {
		fail(w, err)
		return
	}
	s.changed(r, "tenant.delete", "Deleted tenant "+t.Slug+" with its keys and quotas")
	w.WriteHeader(http.StatusNoContent)
}

// createKey is the only response that ever contains the key itself.
func (s *Server) createKey(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Name string `json:"name"`
	}
	if err := decode(r, &b); err != nil {
		fail(w, err)
		return
	}
	k, plain, err := s.st.CreateKey(r.Context(), r.PathValue("id"), b.Name)
	if err != nil {
		fail(w, err)
		return
	}
	s.changed(r, "key.add", "Created API key "+k.ClientID)
	writeJSON(w, http.StatusCreated, map[string]any{"key": k, "secret": plain})
}

func (s *Server) revokeKey(w http.ResponseWriter, r *http.Request) {
	clientID, err := s.st.RevokeKey(r.Context(), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	s.changed(r, "key.revoke", "Revoked API key "+clientID)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) upsertQuota(w http.ResponseWriter, r *http.Request) {
	var b struct {
		ModelID    string `json:"model_id"`
		TokenLimit int64  `json:"token_limit"`
		Window     string `json:"window"`
		// Shadow left out keeps the stored setting; a new quota is enforced.
		Shadow *bool `json:"shadow"`
	}
	if err := decode(r, &b); err != nil {
		fail(w, err)
		return
	}
	switch {
	case b.ModelID == "":
		fail(w, invalid("model_id is required"))
		return
	case b.TokenLimit < 1 || b.TokenLimit > render.MaxLimit:
		fail(w, invalid("token_limit must be between 1 and %d, the most the gateway can count in one window", render.MaxLimit))
		return
	case !validWindow(b.Window):
		fail(w, invalid("window must be 1m, 1h or 1d"))
		return
	}
	// A quota on a model with nothing to attach it to would silently do nothing.
	capable, err := s.st.QuotaCapable(r.Context(), b.ModelID)
	if err != nil {
		fail(w, err)
		return
	}
	if !capable {
		fail(w, invalid("this model has no AIServiceBackend on any cluster, so a quota cannot be applied to it"))
		return
	}
	id := r.PathValue("id")
	if err := s.st.UpsertQuota(r.Context(), id, b.ModelID, b.TokenLimit, b.Window, b.Shadow); err != nil {
		fail(w, err)
		return
	}
	quotas, err := s.st.ListQuotas(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	s.changed(r, "quota.set", s.quotaSummary(r.Context(), id, b.ModelID, quotas))
	writeJSON(w, http.StatusOK, quotas)
}

func (s *Server) deleteQuota(w http.ResponseWriter, r *http.Request) {
	label, err := s.st.DeleteQuota(r.Context(), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	s.changed(r, "quota.delete", "Removed quota of "+label)
	w.WriteHeader(http.StatusNoContent)
}

var windowNames = map[string]string{"1m": "minute", "1h": "hour", "1d": "day"}

// quotaSummary describes the quota that was just set, for the task log.
func (s *Server) quotaSummary(ctx context.Context, tenantID, modelID string, quotas []store.Quota) string {
	t, err := s.st.GetTenant(ctx, tenantID)
	if err != nil {
		return "Set a quota"
	}
	for _, q := range quotas {
		if q.ModelID != modelID {
			continue
		}
		summary := fmt.Sprintf("Set quota of %s on %s to %s per %s", t.Slug, q.ModelName, store.Amount(q.TokenLimit, q.Unit), windowNames[q.Window])
		if q.Shadow {
			summary += " (dry run)"
		}
		return summary
	}
	return "Set a quota for " + t.Slug
}
