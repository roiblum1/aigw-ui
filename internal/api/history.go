package api

import (
	"net/http"
	"strconv"
	"time"

	"aigw-ui/internal/store"
)

// Most days of history one request answers with, by step.
const (
	maxHistoryDays      = 400
	maxHistoryDaysHours = 31
)

// historyFrom reads ?step= and ?days= and returns the step and the start of
// the history asked for: the start of a day in UTC, days-1 days before today.
func historyFrom(r *http.Request, now time.Time) (string, time.Time, error) {
	step := r.URL.Query().Get("step")
	if step == "" {
		step = store.StepDay
	}
	if step != store.StepDay && step != store.StepHour {
		return "", time.Time{}, invalid("step must be day or hour")
	}
	most := maxHistoryDays
	if step == store.StepHour {
		most = maxHistoryDaysHours
	}
	days := 30
	if step == store.StepHour {
		days = 2
	}
	if v := r.URL.Query().Get("days"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > most {
			return "", time.Time{}, invalid("days must be a number from 1 to %d for step %s", most, step)
		}
		days = n
	}
	today := now.UTC().Truncate(24 * time.Hour)
	return step, today.AddDate(0, 0, 1-days), nil
}

type historyResponse struct {
	// From is the start of the first step asked for. Steps in which nothing
	// was used are left out of Points.
	From   time.Time          `json:"from"`
	Step   string             `json:"step"`
	Points []store.UsagePoint `json:"points"`
}

// usageHistory reports what every tenant used of every model per day or per
// hour. ?step=day|hour, ?days= and ?tenant_id= narrow it.
func (s *Server) usageHistory(w http.ResponseWriter, r *http.Request) {
	step, from, err := historyFrom(r, time.Now())
	if err != nil {
		fail(w, err)
		return
	}
	points, err := s.st.UsageHistory(r.Context(), from, step, r.URL.Query().Get("tenant_id"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, historyResponse{From: from, Step: step, Points: points})
}
