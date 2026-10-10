package api

import (
	"errors"
	"math"
	"net/http"
	"time"

	"aigw-ui/internal/render"
	"aigw-ui/internal/store"
	"aigw-ui/internal/syncer"
)

// maxPrice is a thousand dollars for a million tokens, in credits. It is
// there to catch a price typed in the wrong unit.
const maxPrice = 1000 * render.CreditsPerDollar

// priceBody is the body of setting a model's prices, in dollars for a
// million tokens.
type priceBody struct {
	Input  float64 `json:"input_usd"`
	Cached float64 `json:"cached_usd"`
	Output float64 `json:"output_usd"`
	// Now starts the prices at once, not at the next 00:00 UTC. It is only
	// allowed for a model without tenant quotas.
	Now  bool   `json:"now"`
	Note string `json:"note"`
}

// price turns the body into prices in credits that start at the next day.
func (b priceBody) price(now time.Time) (store.Price, error) {
	p := store.Price{Note: b.Note, EffectiveAt: syncer.NextDay(now)}
	for _, f := range []struct {
		name    string
		dollars float64
		credits *int64
		min     int64
	}{
		{"input_usd", b.Input, &p.Input, 1},
		{"cached_usd", b.Cached, &p.Cached, 0},
		{"output_usd", b.Output, &p.Output, 0},
	} {
		credits := math.Round(f.dollars * render.CreditsPerDollar)
		switch {
		case math.IsNaN(credits) || credits < float64(f.min):
			return p, invalid("%s must be at least %s", f.name, syncer.Dollars(f.min))
		case credits > maxPrice:
			return p, invalid("%s is a price for a million tokens and can be at most %s", f.name, syncer.Dollars(maxPrice))
		}
		*f.credits = int64(credits)
	}
	switch {
	case p.Cached > p.Input:
		return p, invalid("cached_usd cannot be more than input_usd: a cached prompt token costs less to serve, not more")
	case len(b.Note) > 500:
		return p, invalid("note must be at most 500 characters")
	}
	return p, nil
}

func (s *Server) listPrices(w http.ResponseWriter, r *http.Request) {
	prices, err := s.st.ListPrices(r.Context(), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, prices)
}

// setPrices stores the prices a model gets from the next 00:00 UTC, in
// place of any that were waiting.
func (s *Server) setPrices(w http.ResponseWriter, r *http.Request) {
	var b priceBody
	if err := decode(r, &b); err != nil {
		fail(w, err)
		return
	}
	p, err := b.price(time.Now())
	if err != nil {
		fail(w, err)
		return
	}
	id := r.PathValue("id")
	name, err := s.st.SetPendingPrice(r.Context(), id, p, b.Now)
	switch {
	case errors.Is(err, store.ErrHasQuotas):
		writeError(w, http.StatusConflict, name+" has tenant quotas, so its prices can only start at 00:00 UTC: the counters of the running windows would otherwise hold amounts at two prices.")
		return
	case err != nil:
		fail(w, err)
		return
	}
	if b.Now {
		if _, err := s.sy.ApplyPrices(r.Context()); err != nil {
			fail(w, err)
			return
		}
		note(r, "model.prices", "Set the prices of "+name+", starting now")
	} else {
		// Nothing changes on the clusters before the day.
		summary := "Set the prices of " + name + " from " + p.EffectiveAt.Format("2006-01-02 15:04 UTC")
		note(r, "model.prices", summary)
		s.sy.Record(r.Context(), "model.prices", summary, "")
	}
	s.writePrices(w, r, id)
}

func (s *Server) deletePendingPrices(w http.ResponseWriter, r *http.Request) {
	name, err := s.st.DeletePendingPrice(r.Context(), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	summary := "Dropped the prices that were waiting for " + name
	note(r, "model.prices", summary)
	s.sy.Record(r.Context(), "model.prices", summary, "")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) writePrices(w http.ResponseWriter, r *http.Request, modelID string) {
	prices, err := s.st.ListPrices(r.Context(), modelID)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, prices)
}

// setPriceDryRun turns dry-run of a priced model on or off. Off is what
// starts refusing tenants by their limits in money.
func (s *Server) setPriceDryRun(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Enabled bool `json:"enabled"`
	}
	if err := decode(r, &b); err != nil {
		fail(w, err)
		return
	}
	name, err := s.st.SetPriceDryRun(r.Context(), r.PathValue("id"), b.Enabled)
	switch {
	case errors.Is(err, store.ErrNotPriced):
		writeError(w, http.StatusConflict, name+" has no prices in use, so there is nothing to try out.")
		return
	case err != nil:
		fail(w, err)
		return
	}
	if b.Enabled {
		s.changed(r, "model.dry-run-on", "Dry-run on for "+name+": every tenant is counted and nobody is refused.")
	} else {
		s.changed(r, "model.dry-run-off", "Dry-run off for "+name+": its quotas are enforced in money.")
	}
	w.WriteHeader(http.StatusNoContent)
}
