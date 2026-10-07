// Package gateway talks to an AI gateway's own HTTP API.
package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

var client = &http.Client{Timeout: 10 * time.Second}

// ListModels returns the model names the gateway at baseURL serves, from its
// OpenAI-compatible /v1/models endpoint. token is sent as a bearer token when
// it is not empty.
func ListModels(ctx context.Context, baseURL, token string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(baseURL, "/")+"/v1/models", nil)
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET /v1/models returned %s", resp.Status)
	}
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("GET /v1/models did not return a model list: %w", err)
	}

	seen := map[string]bool{}
	names := make([]string, 0, len(list.Data))
	for _, m := range list.Data {
		if m.ID != "" && !seen[m.ID] {
			seen[m.ID] = true
			names = append(names, m.ID)
		}
	}
	sort.Strings(names)
	return names, nil
}

// NormalizeURL checks a gateway address and returns it as scheme://host[:port].
// A path is rejected rather than dropped so a pasted ".../v1/models" is noticed.
func NormalizeURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("not a valid URL")
	}
	switch {
	case u.Scheme != "http" && u.Scheme != "https":
		return "", fmt.Errorf("must start with http:// or https://")
	case u.Host == "":
		return "", fmt.Errorf("must include a host")
	case u.User != nil:
		return "", fmt.Errorf("must not contain a user name or password")
	case (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "":
		return "", fmt.Errorf("must be the address only, without a path such as /v1/models")
	}
	return u.Scheme + "://" + u.Host, nil
}
