package api

import (
	"errors"
	"net/http"
	"strings"

	"aigw-ui/internal/render"
	"aigw-ui/internal/store"
)

// errBudgetNeedsTokens is why a tenant cannot have a budget across models or
// a list of models yet.
var errBudgetNeedsTokens = errors.New("a budget across models, and a list of models a tenant may call, need every tenant on tokens: the gateway reads the tenant from the token, and a request with an API key names none. " +
	"Give every tenant a token and revoke its API keys first")

// tokensOnly answers 409 and returns false unless the gateways take tokens
// only. It asks what the gateways take now, not what a new key would change:
// a model's standard or another tenant's budget does not stand in the way.
func (s *Server) tokensOnly(w http.ResponseWriter, r *http.Request) bool {
	mode, err := s.st.IdentityMode(r.Context())
	if err != nil {
		fail(w, err)
		return false
	}
	if mode != render.IdentityTokens {
		writeError(w, http.StatusConflict, errBudgetNeedsTokens.Error()+".")
		return false
	}
	return true
}

// setTenantBudget sets the tenant's budget across every priced model; a
// null amount removes it. It is enforced by the gateways from the next sync,
// and what the tenant used so far in the window stays counted while the
// amount alone changes; see the user guide for when a count starts over.
func (s *Server) setTenantBudget(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Credits *int64 `json:"credits"`
		Window  string `json:"window"`
	}
	if err := decode(r, &b); err != nil {
		fail(w, err)
		return
	}
	var budget *store.TenantBudget
	if b.Credits != nil {
		switch {
		case *b.Credits < 1 || *b.Credits > render.MaxLimit:
			fail(w, invalid("credits must be between 1 and %d, the most the gateway can count in one window", render.MaxLimit))
			return
		case !validWindow(b.Window):
			fail(w, invalid("window must be 1m, 1h or 1d"))
			return
		}
		if !s.tokensOnly(w, r) {
			return
		}
		budget = &store.TenantBudget{Credits: *b.Credits, Window: b.Window}
	}
	t, err := s.st.SetTenantBudget(r.Context(), r.PathValue("id"), budget)
	if err != nil {
		fail(w, err)
		return
	}
	if budget == nil {
		s.changed(r, "tenant.budget", "Removed the budget across models of "+t.Slug)
	} else {
		s.changed(r, "tenant.budget", "Set the budget across models of "+t.Slug+" to "+render.Dollars(budget.Credits)+" per "+windowNames[budget.Window])
	}
	writeJSON(w, http.StatusOK, t)
}

// setTenantModels sets which models the tenant may call: a list of model
// IDs, or null for every model. The gateways refuse any other model from
// the next sync.
func (s *Server) setTenantModels(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Models []string `json:"models"`
	}
	if err := decode(r, &b); err != nil {
		fail(w, err)
		return
	}
	if b.Models != nil && !s.tokensOnly(w, r) {
		return
	}
	t, err := s.st.SetTenantModels(r.Context(), r.PathValue("id"), b.Models)
	if err != nil {
		fail(w, err)
		return
	}
	switch {
	case t.Models == nil:
		s.changed(r, "tenant.models", t.Slug+" may call every model")
	case len(t.Models) == 0:
		s.changed(r, "tenant.models", t.Slug+" may call no model")
	default:
		names, err := s.modelNames(r, t.Models)
		if err != nil {
			fail(w, err)
			return
		}
		s.changed(r, "tenant.models", t.Slug+" may call only "+strings.Join(names, ", "))
	}
	writeJSON(w, http.StatusOK, t)
}

// modelNames returns the names of the models with the given IDs, in order.
func (s *Server) modelNames(r *http.Request, ids []string) ([]string, error) {
	models, err := s.st.ListModels(r.Context())
	if err != nil {
		return nil, err
	}
	byID := make(map[string]string, len(models))
	for _, m := range models {
		byID[m.ID] = m.Name
	}
	names := make([]string, 0, len(ids))
	for _, id := range ids {
		if name, ok := byID[id]; ok {
			names = append(names, name)
		}
	}
	return names, nil
}
