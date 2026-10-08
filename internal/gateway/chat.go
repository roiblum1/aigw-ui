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
	// Body is the start of the response body, for an error message.
	Body string
}

// Chat sends the smallest possible chat completion for model through the
// gateway at baseURL. key is sent as a bearer token when it is not empty. An
// HTTP error status is not an error: it is returned in the result.
func Chat(ctx context.Context, baseURL, key, model string) (ChatResult, error) {
	body, err := json.Marshal(map[string]any{
		"model":      model,
		"messages":   []map[string]string{{"role": "user", "content": "Reply with OK."}},
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
	resp, err := chatClient.Do(req)
	if err != nil {
		return ChatResult{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return ChatResult{}, err
	}
	res := ChatResult{Status: resp.StatusCode, Body: strings.TrimSpace(string(raw))}
	if len(res.Body) > 300 {
		res.Body = res.Body[:300] + "…"
	}
	var answer struct {
		Usage struct {
			TotalTokens int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if resp.StatusCode == http.StatusOK && json.Unmarshal(raw, &answer) == nil {
		res.Tokens = answer.Usage.TotalTokens
	}
	return res, nil
}
