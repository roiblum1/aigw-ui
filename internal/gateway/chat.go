package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

// A model that has to be loaded first can take a while to answer.
var chatClient = &http.Client{Timeout: 60 * time.Second}

// ChatResult is the gateway's answer to one chat request.
type ChatResult struct {
	Status int
	// Tokens is usage.total_tokens of a successful answer, 0 when absent.
	Tokens int64
	// PromptTokens and CompletionTokens are the two parts of it.
	PromptTokens, CompletionTokens int64
	// CachedTokens is how much of the prompt the model took from its prefix
	// cache. It is nil when the answer does not say: the model server then
	// does not report it, and a discount for cached prompts cannot work.
	CachedTokens *int64
	// Body is the start of the response body, for an error message.
	Body string
	// ServedBy is the site that served the request, from the response
	// header of that name. Empty when the answer does not carry it.
	ServedBy string
}

// ServedByHeader is set on the response by the site that served a request.
const ServedByHeader = "x-llm-served-by"

// testPrompt is long enough to fill several blocks of a model's prefix
// cache, so a second request with it can be a cache hit, and to cost more
// than nothing when requests are priced. The answer is still one token.
var testPrompt = strings.Repeat("This is a test request from the gateway's control plane. It checks that an API key is accepted, that usage is counted for the right tenant and that a quota refuses. ", 8) + "Reply with OK."

// Chat sends a chat completion with a one-token answer for model through the
// gateway at baseURL. key is sent as a bearer token when it is not empty. An
// HTTP error status is not an error: it is returned in the result.
func Chat(ctx context.Context, baseURL, key, model string) (ChatResult, error) {
	return ChatWith(ctx, baseURL, key, model, nil)
}

// ChatWith is Chat with extra request headers.
func ChatWith(ctx context.Context, baseURL, key, model string, headers map[string]string) (ChatResult, error) {
	body, err := json.Marshal(map[string]any{
		"model":      model,
		"messages":   []map[string]string{{"role": "user", "content": testPrompt}},
		"max_tokens": 1,
	})
	if err != nil {
		return ChatResult{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(baseURL, "/")+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return ChatResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	resp, err := chatClient.Do(req)
	if err != nil {
		return ChatResult{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return ChatResult{}, err
	}
	res := ChatResult{Status: resp.StatusCode, Body: strings.TrimSpace(string(raw)), ServedBy: resp.Header.Get(ServedByHeader)}
	if len(res.Body) > 300 {
		res.Body = res.Body[:300] + "…"
	}
	var answer struct {
		Usage struct {
			TotalTokens      int64 `json:"total_tokens"`
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
			Details          *struct {
				CachedTokens *int64 `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if resp.StatusCode == http.StatusOK && json.Unmarshal(raw, &answer) == nil {
		u := answer.Usage
		res.Tokens, res.PromptTokens, res.CompletionTokens = u.TotalTokens, u.PromptTokens, u.CompletionTokens
		if u.Details != nil {
			res.CachedTokens = u.Details.CachedTokens
		}
	}
	return res, nil
}
