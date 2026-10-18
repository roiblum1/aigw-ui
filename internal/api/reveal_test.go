package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"aigw-ui/internal/store"
)

// An admin sees a token in full again, as it was signed, in no cache and
// with an audit note; a personal key, stored only as a hash, and a revoked
// token are refused with 409, an unknown key with 404.
func TestRevealKey(t *testing.T) {
	st, ctx := openStore(t)
	st.SetTokens(store.TokenSettings{Issuer: "aigw-ui", Audience: "aigw-fleet", Lifetime: 24 * time.Hour, RenewedLifetime: 7 * 24 * time.Hour})
	s := &Server{st: st}
	a, err := st.CreateTenant(ctx, "team-a", "")
	if err != nil {
		t.Fatal(err)
	}
	tok, signed, err := st.CreateToken(ctx, a.ID, "ci", st.Tokens(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	personal, _, err := st.CreatePersonalKey(ctx, a.ID, "laptop")
	if err != nil {
		t.Fatal(err)
	}
	reveal := func(id string) (*httptest.ResponseRecorder, *auditNote) {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/api/v1/keys/"+id+"/reveal", nil)
		r.SetPathValue("id", id)
		n := &auditNote{}
		s.revealKey(w, r.WithContext(context.WithValue(r.Context(), auditKey{}, n)))
		return w, n
	}

	w, n := reveal(tok.ID)
	var out struct {
		Key    store.APIKey `json:"key"`
		Secret string       `json:"secret"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); w.Code != http.StatusOK || err != nil || out.Secret != signed || out.Key.ClientID != tok.ClientID {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if w.Header().Get("Cache-Control") != "no-store" || n.action != "key.reveal" || n.summary != "Showed token "+tok.ClientID {
		t.Fatalf("cache %q, note %+v", w.Header().Get("Cache-Control"), n)
	}

	if w, _ := reveal(personal.ID); w.Code != http.StatusConflict {
		t.Fatalf("personal key: %d %s", w.Code, w.Body)
	}
	if _, err := st.RevokeKey(ctx, tok.ID); err != nil {
		t.Fatal(err)
	}
	if w, _ := reveal(tok.ID); w.Code != http.StatusConflict {
		t.Fatalf("revoked token: %d %s", w.Code, w.Body)
	}
	if w, _ := reveal("00000000-0000-0000-0000-000000000000"); w.Code != http.StatusNotFound {
		t.Fatalf("unknown key: %d %s", w.Code, w.Body)
	}
}
