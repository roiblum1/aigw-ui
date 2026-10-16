package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"aigw-ui/internal/store"
)

// A personal key gets a token that is not cached; an unknown key gets 401,
// and after ten wrong keys in a minute the address gets 429, a right key
// included.
func TestRenewToken(t *testing.T) {
	st, ctx := openStore(t)
	st.SetTokens(store.TokenSettings{Issuer: "aigw-ui", Audience: "aigw-fleet", Lifetime: 24 * time.Hour, RenewedLifetime: 7 * 24 * time.Hour})
	s := &Server{st: st}
	a, err := st.CreateTenant(ctx, "team-a", "")
	if err != nil {
		t.Fatal(err)
	}
	_, secret, err := st.CreatePersonalKey(ctx, a.ID, "laptop")
	if err != nil {
		t.Fatal(err)
	}
	renew := func(key string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/api/v1/token", nil)
		r.RemoteAddr = "10.0.0.7:5000"
		r.Header.Set("Authorization", "Bearer "+key)
		s.renewToken(w, r)
		return w
	}
	w := renew(secret)
	var out struct {
		Token     string `json:"token"`
		ExpiresIn int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); w.Code != http.StatusOK || err != nil || out.Token == "" {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if out.ExpiresIn < 7*24*3600-2 || out.ExpiresIn > 7*24*3600 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("expires_in %d, cache %q", out.ExpiresIn, w.Header().Get("Cache-Control"))
	}
	for i := range 10 {
		if w := renew(store.PersonalKeyPrefix + "wrong"); w.Code != http.StatusUnauthorized {
			t.Fatalf("wrong key %d: %d", i, w.Code)
		}
	}
	if w := renew(secret); w.Code != http.StatusTooManyRequests {
		t.Fatalf("after ten wrong keys: %d", w.Code)
	}
}
