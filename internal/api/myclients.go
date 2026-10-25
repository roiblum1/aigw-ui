package api

import (
	"cmp"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"aigw-ui/clients"
	"aigw-ui/internal/market"
	"aigw-ui/internal/store"
)

// What a tenant's clients are set up with: the gateway's addresses and the
// models the tenant may call. The installer and the Oh My Pi extension ask
// for it with the tenant's personal key, so that nobody types an address or
// a model name.

// myClients is the answer of GET /api/v1/my/models.
type myClients struct {
	// GatewayURL is the gateway's address without an API's path, and the
	// two base URLs are the APIs on it: Anthropic's, which Claude Code
	// takes as ANTHROPIC_BASE_URL, and OpenAI's. All three are "" when the
	// hub is not told the address (config.gatewayURL).
	GatewayURL       string `json:"gateway_url"`
	AnthropicBaseURL string `json:"anthropic_base_url"`
	OpenAIBaseURL    string `json:"openai_base_url"`
	// ClaudeCode is what Claude Code is set up to ask for, nil when the
	// tenant may call no model.
	ClaudeCode *myClaudeCode   `json:"claude_code"`
	Models     []myClientModel `json:"models"`
	// The version of the client files the hub serves (clients.Version), which
	// the installer keeps so that aigw-token can tell when to run it again.
	ClientsVersion string `json:"clients_version"`
}

// myClaudeCode is the model Claude Code works with and the one it uses for
// its small background tasks, such as naming a conversation.
type myClaudeCode struct {
	Model      string `json:"model"`
	SmallModel string `json:"small_model"`
}

// myClientModel is a model the tenant may call, with what a client needs
// to send it requests that fit.
type myClientModel struct {
	// Name is what a request names: the model the gateway routes on.
	Name string `json:"name"`
	// ContextWindow is the most tokens a request and its answer may hold:
	// the least its deployments declare in aigw-ui.io/max-model-len, or the
	// market's figure for the model. 0 when neither is known.
	ContextWindow int64 `json:"context_window"`
	// Prices are in dollars per million tokens; nil while it has none.
	Prices *myDollarPrices `json:"prices"`
}

type myDollarPrices struct {
	Input  float64 `json:"input"`
	Cached float64 `json:"cached"`
	Output float64 `json:"output"`
}

// creditsPerDollar is what a dollar is in credits.
const creditsPerDollar = 100_000

// getMyModels answers a tenant with the gateway's addresses and the models
// it may call, by name.
func (s *Server) getMyModels(w http.ResponseWriter, r *http.Request) {
	t := r.Context().Value(tenantKey{}).(store.Tenant)
	models, err := s.st.ListModels(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	out := myClients{GatewayURL: s.gatewayURL, Models: []myClientModel{}, ClientsVersion: clients.Version()}
	if s.gatewayURL != "" {
		out.AnthropicBaseURL, out.OpenAIBaseURL = s.gatewayURL+"/anthropic", s.gatewayURL+"/v1"
	}
	snap := market.Current()
	var callable []store.Model
	for _, m := range models {
		// A model with no endpoint is served nowhere.
		if len(m.Endpoints) == 0 || (t.Models != nil && !slices.Contains(t.Models, m.ID)) {
			continue
		}
		callable = append(callable, m)
		c := myClientModel{Name: m.Name, ContextWindow: declaredLength(m)}
		if e, _, _, ok := marketEntry(m, snap); ok && c.ContextWindow == 0 {
			c.ContextWindow = e.Context
		}
		if p := m.Prices; p != nil {
			c.Prices = &myDollarPrices{Input: dollars(p.Input), Cached: dollars(p.Cached), Output: dollars(p.Output)}
		}
		out.Models = append(out.Models, c)
	}
	if len(callable) > 0 {
		out.ClaudeCode = &myClaudeCode{
			Model:      chooseModel(callable, s.claudeModel, priciest),
			SmallModel: chooseModel(callable, s.claudeSmallModel, cheapest),
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func dollars(credits int64) float64 { return float64(credits) / creditsPerDollar }

// declaredLength is the least length the model's deployments declare, 0
// when none does. A site lists several when its deployments differ.
func declaredLength(m store.Model) int64 {
	var least int64
	for _, e := range m.Endpoints {
		for v := range strings.SplitSeq(e.Capacity.MaxModelLen, ",") {
			n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			if err == nil && n > 0 && (least == 0 || n < least) {
				least = n
			}
		}
	}
	return least
}

// chooseModel is the model an operator chose, when the tenant may call it,
// or the first of the tenant's models by order: those with prices first.
func chooseModel(models []store.Model, chosen string, order func(a, b store.Model) int) string {
	for _, m := range models {
		if chosen != "" && strings.EqualFold(m.Name, chosen) {
			return m.Name
		}
	}
	return slices.MinFunc(models, func(a, b store.Model) int {
		if (a.Prices == nil) != (b.Prices == nil) {
			if a.Prices == nil {
				return 1
			}
			return -1
		}
		if a.Prices != nil {
			if c := order(a, b); c != 0 {
				return c
			}
		}
		return cmp.Compare(a.Name, b.Name)
	}).Name
}

// priciest orders models by their output price, then their input price,
// highest first; cheapest lowest first. Both are only given priced models.
func priciest(a, b store.Model) int { return -cheapest(a, b) }

func cheapest(a, b store.Model) int {
	return cmp.Or(cmp.Compare(a.Prices.Output, b.Prices.Output), cmp.Compare(a.Prices.Input, b.Prices.Input))
}
