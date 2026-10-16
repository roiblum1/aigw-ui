package api

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"aigw-ui/internal/store"
)

// renewToken trades a personal key for a new token, signed with the
// tenant's budget across models and its models as they are now. A user's
// helper asks for one well before the last runs out, so a change reaches
// the gateway with the next token and the user never handles one. Wrong
// keys count against the address as on the tenants' page. No request to a
// model passes through here: the hub only signs.
func (s *Server) renewToken(w http.ResponseWriter, r *http.Request) {
	addr, now := clientAddr(r), time.Now()
	if s.keyFailures.blocked(addr, now) {
		writeError(w, http.StatusTooManyRequests, "too many wrong keys; try again in a minute")
		return
	}
	key, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	plain, expires, t, err := s.st.RenewToken(r.Context(), strings.TrimSpace(key), s.st.Tokens(), now)
	if errors.Is(err, store.ErrNotFound) {
		s.keyFailures.add(addr, now)
		slog.Warn("token renewal: wrong key", "remote_addr", addr)
		writeError(w, http.StatusUnauthorized, "that is not a personal key of an enabled tenant")
		return
	}
	if err != nil {
		fail(w, err)
		return
	}
	// Every user renews several times an hour: worth a debug line only.
	slog.Debug("token renewed", "tenant", t.Slug)
	// A token is a credential: no cache may keep it (RFC 6749, 5.1).
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"token":      plain,
		"expires_at": expires,
		"expires_in": int64(expires.Sub(now).Seconds()),
	})
}
