package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// serve answers every completion with the given status and body, and keeps the
// last request body.
func serve(t *testing.T, status int, body string) (*Client, *string) {
	t.Helper()
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		got = string(b)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return New(Config{BaseURL: srv.URL + "/", Model: "m", Timeout: time.Second, MaxTokens: 64}), &got
}

func TestCompleteToolCalls(t *testing.T) {
	// llama-server's shape: arguments as a string, plus its own timings.
	c, sent := serve(t, 200, `{
	  "choices": [{"message": {"content": null, "tool_calls": [
	    {"type": "function", "function": {"name": "report", "arguments": "{\"message\": \"hi\"}"}},
	    {"type": "function", "function": {"name": "do_nothing", "arguments": {}}}
	  ]}}],
	  "usage": {"prompt_tokens": 120, "completion_tokens": 18},
	  "timings": {"predicted_per_second": 42.5}
	}`)
	resp, err := c.Complete(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hello"}},
		Tools:    []Tool{{Name: "report", Description: "tell", Parameters: map[string]any{"type": "object"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.ToolCalls) != 2 || resp.ToolCalls[0].Name != "report" ||
		string(resp.ToolCalls[0].Arguments) != `{"message": "hi"}` || string(resp.ToolCalls[1].Arguments) != `{}` {
		t.Errorf("tool calls = %+v", resp.ToolCalls)
	}
	if resp.Content != "" || resp.PromptTokens != 120 || resp.CompletionTokens != 18 || resp.TokensPerSecond != 42.5 || resp.Latency <= 0 {
		t.Errorf("response = %+v", resp)
	}

	var req map[string]any
	if err := json.Unmarshal([]byte(*sent), &req); err != nil {
		t.Fatal(err)
	}
	tools, _ := req["tools"].([]any)
	kwargs, _ := req["chat_template_kwargs"].(map[string]any)
	if req["model"] != "m" || req["stream"] != false || req["max_tokens"] != float64(64) || len(tools) != 1 ||
		kwargs["enable_thinking"] != false || resp.Truncated {
		t.Errorf("request = %s", *sent)
	}
	if fn := tools[0].(map[string]any)["function"].(map[string]any); fn["name"] != "report" || fn["parameters"] == nil {
		t.Errorf("tool on the wire = %v", fn)
	}
}

func TestCompleteTextOnly(t *testing.T) {
	c, _ := serve(t, 200, `{"choices": [{"message": {"content": "I will wait."}}], "usage": {"completion_tokens": 4}}`)
	resp, err := c.Complete(context.Background(), Request{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "I will wait." || len(resp.ToolCalls) != 0 || resp.TokensPerSecond <= 0 {
		t.Errorf("response = %+v", resp)
	}
}

func TestCompleteTruncatedAndThinking(t *testing.T) {
	c, sent := serve(t, 200, `{"choices": [{"finish_reason": "length", "message": {"content": "", "tool_calls": [
	    {"function": {"name": "report", "arguments": "{\"message\": \"Safety lock"}}]}}]}`)
	c.cfg.Thinking = true
	resp, err := c.Complete(context.Background(), Request{})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Truncated || len(resp.ToolCalls) != 1 {
		t.Errorf("response = %+v, want it marked truncated with the cut-off call kept", resp)
	}
	// With thinking on, the model's own default is left alone.
	if strings.Contains(*sent, "chat_template_kwargs") {
		t.Errorf("request = %s", *sent)
	}
}

func TestCompleteErrors(t *testing.T) {
	for name, tt := range map[string]struct {
		status int
		body   string
		want   string
	}{
		"http error": {500, "model not loaded", "model not loaded"},
		"not json":   {200, "<html>", "decode"},
		"no choices": {200, `{"choices": []}`, "no choices"},
	} {
		t.Run(name, func(t *testing.T) {
			c, _ := serve(t, tt.status, tt.body)
			if _, err := c.Complete(context.Background(), Request{}); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want one mentioning %q", err, tt.want)
			}
		})
	}
}

func TestCompleteTimeout(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-block }))
	defer srv.Close()
	defer close(block)
	c := New(Config{BaseURL: srv.URL, Timeout: 30 * time.Millisecond})
	if _, err := c.Complete(context.Background(), Request{}); err == nil {
		t.Error("want an error once the timeout passes")
	}
}

func TestArgumentsKeepsBrokenJSON(t *testing.T) {
	// What the model wrote reaches the caller as it is; judging it is not this
	// package's job.
	if got := string(arguments(json.RawMessage(`"{\"location\": "`))); got != `{"location": ` {
		t.Errorf("got %q", got)
	}
}
