// Package anthropic is a minimal Anthropic Messages API client used
// by (LLM-assisted root-cause analysis). Intentionally
// dependency-free: a tiny wrapper over net/http rather than the
// official anthropic-sdk-go, so the backend binary stays small and
// the dependency surface stays auditable.
//
// Scope:
//
//   - Single endpoint: POST /v1/messages
//   - Synchronous request/response (no streaming)
//   - System + single user message; tool-use, vision, and
//     extended thinking are out of scope for the root-cause
//     analysis use case.
//   - Caller supplies the model id, the system prompt, the user
//     prompt, max_tokens, and (optionally) temperature. We do not
//     hard-code a model so the caller can choose Haiku (cheap) vs
//     Sonnet (better quality) per use case.
//
// The client is disabled when the API key is empty: every Call
// returns ErrDisabled without making a network round trip. This
// lets the backend ship the analysis feature behind an env-var
// flag (ANTHROPIC_API_KEY) so customers who do not configure a key
// see a graceful "analysis is not configured for this deployment"
// message rather than a crash.
package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	// defaultEndpoint is the public Anthropic Messages API URL.
	defaultEndpoint = "https://api.anthropic.com/v1/messages"
	// defaultAPIVersion pins the API version header. Bumping this
	// requires re-verifying the response shape we parse below; the
	// 2023-06-01 contract has been stable for a long time.
	defaultAPIVersion = "2023-06-01"
	// defaultTimeout caps a single Call. Reasoning models can take
	// a while, but a 60 second ceiling is more than enough for the
	// root-cause analysis use case (Haiku usually returns in under
	// 5 seconds).
	defaultTimeout = 60 * time.Second
)

// ErrDisabled is returned by Call when no API key is configured.
// Callers should treat this as a soft failure and surface a "not
// configured" UI rather than an error to the customer.
var ErrDisabled = errors.New("anthropic: client disabled (no API key)")

// Client is a thin Anthropic Messages API client. Safe for
// concurrent use; the underlying net/http.Client is the standard
// connection-pooling shared client.
type Client struct {
	apiKey     string
	endpoint   string
	apiVersion string
	http       *http.Client
}

// New constructs a Client. An empty apiKey produces a disabled
// client (every Call returns ErrDisabled). endpoint and apiVersion
// can be left empty to use the public defaults.
func New(apiKey, endpoint, apiVersion string) *Client {
	if endpoint == "" {
		endpoint = defaultEndpoint
	}
	if apiVersion == "" {
		apiVersion = defaultAPIVersion
	}
	return &Client{
		apiKey:     apiKey,
		endpoint:   endpoint,
		apiVersion: apiVersion,
		http: &http.Client{
			Timeout: defaultTimeout,
		},
	}
}

// Enabled reports whether the client has an API key configured.
// Hot-path predicate so callers can skip building the prompt when
// nothing will be done with it.
func (c *Client) Enabled() bool {
	return c != nil && c.apiKey != ""
}

// CallOptions controls a single Messages API call.
type CallOptions struct {
	// Model is the Anthropic model id, e.g. "claude-haiku-4-5".
	Model string
	// System is the system prompt. Optional but recommended.
	System string
	// User is the user prompt (single turn; no conversation
	// history is supported in this minimal client).
	User string
	// MaxTokens caps the response length. The API requires this;
	// 1024 is a reasonable default for analysis output.
	MaxTokens int
	// Temperature is in [0, 1]. Leave 0 to use the API default.
	Temperature float64
}

// Result is what the Messages API returned. Caller-visible fields
// only; transport details are dropped.
type Result struct {
	Text         string // joined text blocks from the response
	InputTokens  int
	OutputTokens int
	StopReason   string
}

// requestBody is the JSON shape we POST to /v1/messages.
type requestBody struct {
	Model       string           `json:"model"`
	MaxTokens   int              `json:"max_tokens"`
	System      string           `json:"system,omitempty"`
	Messages    []requestMessage `json:"messages"`
	Temperature *float64         `json:"temperature,omitempty"`
}

type requestMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// responseBody is the relevant subset of the Messages API response.
// We ignore fields we don't use to keep the unmarshal cheap.
type responseBody struct {
	ID         string            `json:"id"`
	Type       string            `json:"type"`
	Role       string            `json:"role"`
	Model      string            `json:"model"`
	Content    []responseContent `json:"content"`
	StopReason string            `json:"stop_reason"`
	Usage      responseUsage     `json:"usage"`
}

type responseContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type responseUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// Call makes a single Messages API call and returns the assembled
// response. Returns ErrDisabled when the client has no API key.
func (c *Client) Call(ctx context.Context, opts CallOptions) (*Result, error) {
	if !c.Enabled() {
		return nil, ErrDisabled
	}
	if opts.Model == "" {
		return nil, fmt.Errorf("anthropic: model is required")
	}
	if opts.MaxTokens <= 0 {
		opts.MaxTokens = 1024
	}
	body := requestBody{
		Model:     opts.Model,
		MaxTokens: opts.MaxTokens,
		System:    opts.System,
		Messages: []requestMessage{
			{Role: "user", Content: opts.User},
		},
	}
	if opts.Temperature > 0 {
		t := opts.Temperature
		body.Temperature = &t
	}

	raw, err := json.Marshal(&body)
	if err != nil {
		return nil, fmt.Errorf("anthropic: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("anthropic: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", c.apiKey)
	req.Header.Set("anthropic-version", c.apiVersion)

	res, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("anthropic: do request: %w", err)
	}
	defer res.Body.Close()

	if res.StatusCode >= 400 {
		body, _ := io.ReadAll(res.Body)
		return nil, fmt.Errorf("anthropic: status %d: %s", res.StatusCode, strings.TrimSpace(string(body)))
	}

	var parsed responseBody
	if err := json.NewDecoder(res.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("anthropic: decode response: %w", err)
	}

	var sb strings.Builder
	for _, c := range parsed.Content {
		if c.Type == "text" {
			sb.WriteString(c.Text)
		}
	}
	return &Result{
		Text:         sb.String(),
		InputTokens:  parsed.Usage.InputTokens,
		OutputTokens: parsed.Usage.OutputTokens,
		StopReason:   parsed.StopReason,
	}, nil
}
