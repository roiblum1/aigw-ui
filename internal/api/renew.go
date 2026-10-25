package api

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"aigw-ui/clients"
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
	got, err := s.st.RenewToken(r.Context(), strings.TrimSpace(key), s.st.Tokens(), now)
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
	slog.Debug("token renewed", "tenant", got.Tenant.Slug)
	if got.Listed {
		// While the gateways take API keys too, they take only the tokens
		// they list. This one is given out once a sync has had time to
		// take it there, unless the key had none to give.
		s.sy.Changed(r.Context(), "key.renew", "Listed a new token of a personal key of "+got.Tenant.Slug+" for the gateways, which take API keys too")
	}
	// A token is a credential: no cache may keep it (RFC 6749, 5.1).
	w.Header().Set("Cache-Control", "no-store")
	// The client files' version rides along: aigw-token runs the installer
	// again when it differs from the one it installed.
	writeJSON(w, http.StatusOK, map[string]any{
		"token":           got.Token,
		"expires_at":      got.Expires,
		"expires_in":      int64(got.Expires.Sub(now).Seconds()),
		"clients_version": clients.Version(),
	})
}
