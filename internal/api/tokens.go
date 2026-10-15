package api

import "net/http"

// signingKeys returns the hub's signing keys, without their private halves,
// and how the gateways tell tenants apart now.
func (s *Server) signingKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := s.st.ListSigningKeys(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	id, err := s.st.Identity(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	set := s.st.Tokens()
	writeJSON(w, http.StatusOK, map[string]any{
		"keys": keys, "identity": id.Mode, "issuer": set.Issuer, "audience": set.Audience,
		"lifetime_days": int(set.Lifetime.Hours() / 24), "denied": len(id.Denied),
	})
}

// rotateSigningKey makes a new signing key, which signs every token from now
// on. Tokens the old one signed keep working until they expire or are
// revoked; reissue them to retire it sooner.
func (s *Server) rotateSigningKey(w http.ResponseWriter, r *http.Request) {
	id, err := s.st.RotateSigningKey(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	s.changed(r, "signing-key.rotate", "Rotated the signing key: "+id+" signs new tokens")
	writeJSON(w, http.StatusOK, map[string]any{"id": id})
}
