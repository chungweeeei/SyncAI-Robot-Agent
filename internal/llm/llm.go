// Package llm talks to a local model through the OpenAI-compatible
// /v1/chat/completions endpoint, which llama-server and Ollama both serve.
package llm

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

// Config says where the model is and how it is asked.
type Config struct {
	BaseURL     string        // e.g. "http://localhost:8080"; "/v1/chat/completions" is appended
	Model       string        // llama-server ignores it (one model per server); Ollama needs it
	Timeout     time.Duration // bound on one completion
	MaxTokens   int
	Temperature float64

	// Thinking lets a reasoning model think before it answers. Off, the request
	// carries chat_template_kwargs {"enable_thinking": false}, which llama-server
	// hands to the model's chat template. Thinking tokens count against
	// MaxTokens: measured on Qwen3.5-4B, they used up a 512-token budget before
	// any tool call was written, and one decision took 10 s instead of 1 s.
	Thinking bool
}

// Message is one chat message.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Tool is a function the model may call. Parameters is its JSON schema.
type Tool struct {
	Name        string
	Description string
	Parameters  map[string]any
}

// ToolCall is one call the model made. Arguments is the JSON object it wrote,
// unparsed: the model is untrusted and the caller validates it.
type ToolCall struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// Request is one completion.
type Request struct {
	Messages []Message
	Tools    []Tool
}

// Response is what the model answered, with what it cost.
type Response struct {
	Content   string
	ToolCalls []ToolCall

	PromptTokens     int
	CompletionTokens int
	// Truncated: the model ran into MaxTokens. Whatever it was writing, a tool
	// call's arguments included, is cut short.
	Truncated bool
	Latency   time.Duration // the whole request, as seen from here
	// TokensPerSecond is the server's own decode rate (llama-server's
	// timings.predicted_per_second) when it reports one; otherwise
	// CompletionTokens over Latency, which also counts prompt processing.
	TokensPerSecond float64
}

// Client calls one endpoint. It is safe for concurrent use.
type Client struct {
	cfg  Config
	http *http.Client
}

// New creates a Client.
func New(cfg Config) *Client {
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	return &Client{cfg: cfg, http: &http.Client{Timeout: cfg.Timeout}}
}

// Model returns the configured model name.
func (c *Client) Model() string { return c.cfg.Model }

// The wire format of /v1/chat/completions, only the fields used.
type (
	wireFunction struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Parameters  map[string]any `json:"parameters"`
	}
	wireTool struct {
		Type     string       `json:"type"`
		Function wireFunction `json:"function"`
	}
	wireRequest struct {
		Model       string     `json:"model,omitempty"`
		Messages    []Message  `json:"messages"`
		Tools       []wireTool `json:"tools,omitempty"`
		MaxTokens   int        `json:"max_tokens,omitempty"`
		Temperature float64    `json:"temperature"`
		Stream      bool       `json:"stream"`
		// A llama-server extension; other servers ignore it.
		ChatTemplateKwargs map[string]any `json:"chat_template_kwargs,omitempty"`
	}
	wireResponse struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content   *string `json:"content"`
				ToolCalls []struct {
					Function struct {
						Name      string          `json:"name"`
						Arguments json.RawMessage `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
		Timings struct {
			PredictedPerSecond float64 `json:"predicted_per_second"`
		} `json:"timings"`
	}
)

// Complete runs one completion.
func (c *Client) Complete(ctx context.Context, req Request) (Response, error) {
	wr := wireRequest{
		Model:       c.cfg.Model,
		Messages:    req.Messages,
		MaxTokens:   c.cfg.MaxTokens,
		Temperature: c.cfg.Temperature,
	}
	if !c.cfg.Thinking {
		wr.ChatTemplateKwargs = map[string]any{"enable_thinking": false}
	}
	for _, t := range req.Tools {
		wr.Tools = append(wr.Tools, wireTool{Type: "function", Function: wireFunction(t)})
	}
	body, err := json.Marshal(wr)
	if err != nil {
		return Response{}, err
	}

	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.BaseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return Response{}, err
	}
	hreq.Header.Set("Content-Type", "application/json")

	start := time.Now()
	resp, err := c.http.Do(hreq)
	if err != nil {
		return Response{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return Response{}, err
	}
	latency := time.Since(start)
	if resp.StatusCode != http.StatusOK {
		return Response{}, fmt.Errorf("llm: %s: %s", resp.Status, truncate(string(raw), 300))
	}

	var w wireResponse
	if err := json.Unmarshal(raw, &w); err != nil {
		return Response{}, fmt.Errorf("llm: decode: %w", err)
	}
	if len(w.Choices) == 0 {
		return Response{}, errors.New("llm: response has no choices")
	}

	msg := w.Choices[0].Message
	out := Response{
		PromptTokens:     w.Usage.PromptTokens,
		CompletionTokens: w.Usage.CompletionTokens,
		Truncated:        w.Choices[0].FinishReason == "length",
		Latency:          latency,
		TokensPerSecond:  w.Timings.PredictedPerSecond,
	}
	if msg.Content != nil {
		out.Content = *msg.Content
	}
	if out.TokensPerSecond == 0 && latency > 0 {
		out.TokensPerSecond = float64(out.CompletionTokens) / latency.Seconds()
	}
	for _, tc := range msg.ToolCalls {
		out.ToolCalls = append(out.ToolCalls, ToolCall{Name: tc.Function.Name, Arguments: arguments(tc.Function.Arguments)})
	}
	return out, nil
}

// arguments unwraps a call's arguments. OpenAI's format carries them as a string
// holding JSON; some servers send the object itself. Whatever is inside is
// returned as it is, valid or not, for the caller to judge.
func arguments(raw json.RawMessage) json.RawMessage {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return json.RawMessage(s)
	}
	return raw
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
