package api

import (
	"net/http"
	"time"

	"aigw-ui/internal/render"
	"aigw-ui/internal/store"
)

// walletBody sets a tenant's wallet in credits per hour or day; a null
// amount removes it.
type walletBody struct {
	Credits *int64 `json:"credits"`
	Window  string `json:"window"`
}

// setWallet sets a tenant's budget across every priced model. Its quotas
// on priced models become caps inside it, and it gets a bucket equal to the
// wallet on each priced model it has no cap on.
func (s *Server) setWallet(w http.ResponseWriter, r *http.Request) {
	var b walletBody
	if err := decode(r, &b); err != nil {
		fail(w, err)
		return
	}
	var wallet *store.Wallet
	if b.Credits != nil {
		if !s.wallets {
			writeError(w, http.StatusConflict, "Wallets are added up from the usage history, which needs Redis (redis.url) and config.usageHistoryInterval; one of them is off, so a wallet would not be enforced.")
			return
		}
		if *b.Credits < 1 || *b.Credits > render.MaxLimit {
			fail(w, invalid("credits must be from 1 to %d", int64(render.MaxLimit)))
			return
		}
		if b.Window != "1h" && b.Window != "1d" {
			fail(w, invalid("window must be 1h or 1d"))
			return
		}
		wallet = &store.Wallet{Credits: *b.Credits, Window: b.Window}
	}
	t, err := s.st.SetWallet(r.Context(), r.PathValue("id"), wallet)
	if err != nil {
		fail(w, err)
		return
	}
	if wallet == nil {
		s.changed(r, "tenant.wallet", t.Slug+" has no wallet: its quotas on priced models are budgets of their own again.")
	} else {
		s.changed(r, "tenant.wallet", t.Slug+" has a wallet of "+store.Amount(wallet.Credits, store.UnitCredits)+" per "+windowName(wallet.Window)+
			" across every priced model; its quotas on them are caps inside it.")
	}
	writeJSON(w, http.StatusOK, t)
}

// walletOut is a tenant's wallet and what it spent of it in the running
// window, by model.
type walletOut struct {
	Credits     int64            `json:"credits"`
	Window      string           `json:"window"`
	Start       time.Time        `json:"start"`
	End         time.Time        `json:"end"`
	Spent       int64            `json:"spent"`
	ByModel     map[string]int64 `json:"by_model"`
	ClosedUntil *time.Time       `json:"closed_until"`
	// Enforced is false when the hub does not add wallets up.
	Enforced bool `json:"enforced"`
}

func (s *Server) getWallet(w http.ResponseWriter, r *http.Request) {
	out, err := s.walletOf(r, r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// walletOf returns a tenant's wallet now, nil without one.
func (s *Server) walletOf(r *http.Request, tenantID string) (*walletOut, error) {
	spends, err := s.st.WalletSpends(r.Context(), time.Now(), tenantID)
	if err != nil || len(spends) == 0 {
		return nil, err
	}
	sp := spends[0]
	return &walletOut{Credits: sp.Wallet.Credits, Window: sp.Wallet.Window, Start: sp.Start, End: sp.End, Spent: sp.Spent,
		ByModel: sp.ByModel, ClosedUntil: sp.ClosedUntil, Enforced: s.wallets}, nil
}
