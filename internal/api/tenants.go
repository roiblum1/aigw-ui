package api

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

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
	if b.Enabled {
		// Enabling a tenant with API keys would make the gateways take API
		// keys again.
		cur, err := s.st.GetTenant(r.Context(), r.PathValue("id"))
		if err != nil {
			fail(w, err)
			return
		}
		if !cur.Enabled && cur.APIKeyCount > 0 {
			if err := s.keepsTokensOnly(r.Context()); err != nil {
				writeError(w, http.StatusConflict, err.Error()+". Revoke the tenant's API keys before enabling it.")
				return
			}
		}
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
// createKey issues a token, the key a tenant pastes as its API key. With
// "kind": "key" it makes an API key of the kind there was before tokens,
// which every gateway lists; the first token moves every gateway to take
// both, until no tenant has an API key left.
func (s *Server) createKey(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Name string `json:"name"`
		Kind string `json:"kind"`
	}
	if err := decode(r, &b); err != nil {
		fail(w, err)
		return
	}
	var k store.APIKey
	var plain string
	var err error
	switch b.Kind {
	case "", store.KindToken:
		k, plain, err = s.st.CreateToken(r.Context(), r.PathValue("id"), b.Name, s.st.Tokens(), time.Now())
	case store.KindPersonal:
		k, plain, err = s.st.CreatePersonalKey(r.Context(), r.PathValue("id"), b.Name)
	case store.KindKey:
		if err = s.keepsTokensOnly(r.Context()); err != nil {
			writeError(w, http.StatusConflict, err.Error()+".")
			return
		}
		k, plain, err = s.st.CreateKey(r.Context(), r.PathValue("id"), b.Name)
	default:
		err = invalid("kind must be %q, %q or %q", store.KindToken, store.KindPersonal, store.KindKey)
	}
	if err != nil {
		fail(w, err)
		return
	}
	s.changed(r, "key.add", "Created "+keyKind(k.Kind)+" "+k.ClientID)
	// The secret is in no cache, and is not compressed (see compress).
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, map[string]any{"key": k, "secret": plain})
}

// keyKind names a kind of key in the activity log.
func keyKind(kind string) string {
	switch kind {
	case store.KindToken:
		return "token"
	case store.KindPersonal:
		return "personal key"
	}
	return "API key"
}

func (s *Server) revokeKey(w http.ResponseWriter, r *http.Request) {
	k, err := s.st.RevokeKey(r.Context(), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	s.changed(r, "key.revoke", "Revoked "+keyKind(k.Kind)+" "+k.ClientID)
	w.WriteHeader(http.StatusNoContent)
}

// revealKey shows an API key or a token again, in full, so that an admin can
// hand it on. It is a POST so that every reveal is in the audit log. A
// personal key cannot be shown: only its hash is stored.
func (s *Server) revealKey(w http.ResponseWriter, r *http.Request) {
	k, secret, err := s.st.KeySecret(r.Context(), r.PathValue("id"))
	switch {
	case err != nil:
		fail(w, err)
		return
	case k.RevokedAt != nil:
		writeError(w, http.StatusConflict, "this "+keyKind(k.Kind)+" is revoked.")
		return
	case secret == "":
		writeError(w, http.StatusConflict, "a "+keyKind(k.Kind)+" is stored only as a hash and cannot be shown again: create a new one.")
		return
	}
	note(r, "key.reveal", "Showed "+keyKind(k.Kind)+" "+k.ClientID)
	// The secret is in no cache, and is not compressed (see compress).
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"key": k, "secret": secret})
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
	id := r.PathValue("id")
	budget, err := s.st.HasBudget(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	if budget {
		writeError(w, http.StatusConflict, errBudgetOrQuotas.Error()+": this tenant has a budget for all models. Remove it first.")
		return
	}
	// A quota on a model the tenant may not call would never be used: the
	// gateway refuses the model first (HTTP 403).
	t, err := s.st.GetTenant(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	if t.Models != nil && !slices.Contains(t.Models, b.ModelID) {
		writeError(w, http.StatusConflict, "this tenant may call only the models in its list, and this model is not one of them: add it to the tenant's models first.")
		return
	}
	// A quota on a model with nothing to attach it to would silently do nothing.
	capable, err := s.st.QuotaCapable(r.Context(), b.ModelID)
	if err != nil {
		fail(w, err)
		return
	}
	if !capable {
		fail(w, invalid("this model has no AIServiceBackend on any cluster and is not a fleet model, so a quota cannot be applied to it"))
		return
	}
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
		return summary + s.budgetSummary(ctx, modelID)
	}
	return "Set a quota for " + t.Slug
}
