package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"aigw-ui/internal/render"
	"aigw-ui/internal/store"
	"aigw-ui/internal/syncer"
)

// standardBody sets a model's standard per-tenant budget; a null limit
// removes it.
type standardBody struct {
	Limit  *int64 `json:"limit"`
	Window string `json:"window"`
}

// errNeedsTokens is why a model cannot have a standard yet.
var errNeedsTokens = errors.New("a standard budget needs every tenant on tokens: the standard's rule reads the tenant header that only a token sets, and a request with an API key has none. " +
	"Give each tenant a token and revoke its API keys first; the Tenants page lists the tenants that still have one")

// setStandard sets the budget every tenant has on a model from the next
// 00:00 UTC, when every quota window starts anew, so that no tenant's count
// starts again mid-window.
func (s *Server) setStandard(w http.ResponseWriter, r *http.Request) {
	var b standardBody
	if err := decode(r, &b); err != nil {
		fail(w, err)
		return
	}
	var st *store.Standard
	if b.Limit != nil {
		if *b.Limit < 1 || *b.Limit > render.MaxLimit {
			fail(w, invalid("limit must be from 1 to %d", int64(render.MaxLimit)))
			return
		}
		if b.Window != "1m" && b.Window != "1h" && b.Window != "1d" {
			fail(w, invalid("window must be 1m, 1h or 1d"))
			return
		}
		// A standard on a model with nothing to attach it to would do nothing.
		capable, err := s.st.QuotaCapable(r.Context(), r.PathValue("id"))
		if err != nil {
			fail(w, err)
			return
		}
		if !capable {
			fail(w, invalid("this model has no AIServiceBackend on any cluster and is not a fleet model, so a standard budget cannot be applied to it"))
			return
		}
		mode, err := s.st.IdentityMode(r.Context())
		if err != nil {
			fail(w, err)
			return
		}
		if mode != render.IdentityTokens {
			writeError(w, http.StatusConflict, errNeedsTokens.Error()+".")
			return
		}
		st = &store.Standard{Limit: *b.Limit, Window: b.Window}
	}
	at := syncer.NextDay(time.Now())
	name, err := s.st.SetStandard(r.Context(), r.PathValue("id"), st, at)
	if err != nil {
		fail(w, err)
		return
	}
	m, err := s.st.GetModel(r.Context(), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	when := " from " + at.Format("2006-01-02") + " 00:00 UTC"
	if st == nil {
		note(r, "model.standard", name+" will have no standard budget"+when+": only tenants with a quota on it can use more than its pool.")
	} else {
		note(r, "model.standard", name+" will give every tenant "+store.Amount(st.Limit, m.Unit)+" per "+windowName(st.Window)+when+".")
	}
	w.WriteHeader(http.StatusNoContent)
}

// cancelStandard drops a change of a model's standard that waits for its day.
func (s *Server) cancelStandard(w http.ResponseWriter, r *http.Request) {
	name, err := s.st.CancelStandard(r.Context(), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	note(r, "model.standard", "The change of "+name+"'s standard budget was cancelled.")
	w.WriteHeader(http.StatusNoContent)
}

// keepsTokensOnly refuses what would make the gateways take API keys again
// while a model has a standard, a tenant has a budget across models or may
// call only some models: their rules read the tenant header a token sets,
// and would leave every request with an API key unheld.
func (s *Server) keepsTokensOnly(ctx context.Context) error {
	any, err := s.st.AnyStandard(ctx)
	if err != nil {
		return err
	}
	if any {
		return errors.New("a model has a standard budget, which needs every tenant on tokens; an API key would leave its requests to the model's pool. Create a token instead")
	}
	if any, err = s.st.AnyBudget(ctx); err != nil || !any {
		return err
	}
	return errors.New("a tenant has a budget across models or a list of models it may call, which need every tenant on tokens; a request with an API key would be held by neither. Create a token instead")
}

func windowName(w string) string {
	switch w {
	case "1m":
		return "minute"
	case "1h":
		return "hour"
	}
	return "day"
}
