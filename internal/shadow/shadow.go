// Package shadow runs the model beside the robot without letting it act: each
// decision is asked for, judged by the guard and written down, and nothing is
// sent to the backend.
package shadow

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/chungweeeei/SyncAI-Robot-Agent/internal/backend"
	"github.com/chungweeeei/SyncAI-Robot-Agent/internal/inputs"
	"github.com/chungweeeei/SyncAI-Robot-Agent/internal/llm"
	"github.com/chungweeeei/SyncAI-Robot-Agent/internal/runtime"
	"github.com/chungweeeei/SyncAI-Robot-Agent/internal/tools"
)

// locationsTTL is how long the stops of a map are kept before they are read
// again; an operator may add one while the agent runs.
const locationsTTL = time.Minute

// Completer is the model; *llm.Client implements it.
type Completer interface {
	Complete(ctx context.Context, req llm.Request) (llm.Response, error)
}

// VertexSource lists the stops of a map; *backend.Client implements it.
type VertexSource interface {
	GetVertices(ctx context.Context, mapName string) ([]backend.Vertex, error)
}

// Call is one tool call with the guard's verdict on it.
type Call struct {
	Name string `json:"name"`
	// Arguments is what the model wrote, kept as text because it may not be JSON.
	Arguments string `json:"arguments"`
	Allowed   bool   `json:"allowed"`
	Refusal   string `json:"refusal,omitempty"`
	// Malformed tells a call that fits no tool's schema from a well-formed one
	// the state forbids.
	Malformed bool `json:"malformed,omitempty"`
}

// Record is one decision, one line of the JSONL log. It carries the prompt and
// the state the calls were judged against, so the same decision can be put to
// another model later and judged the same way.
type Record struct {
	Time    time.Time   `json:"time"`
	Tick    uint64      `json:"tick"`
	Trigger string      `json:"trigger"`
	Reasons []string    `json:"reasons,omitempty"`
	Model   string      `json:"model"`
	Prompt  string      `json:"prompt"`
	State   tools.State `json:"state"`

	Content string `json:"content,omitempty"`
	Calls   []Call `json:"calls,omitempty"`
	Error   string `json:"error,omitempty"` // the completion failed; Calls is empty

	LatencyMS        int64   `json:"latency_ms"`
	PromptTokens     int     `json:"prompt_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	TokensPerSecond  float64 `json:"tokens_per_second"`
}

// Shadow asks the model on a tick and records what it would have done.
type Shadow struct {
	LLM      Completer
	Model    string
	Robot    func() (inputs.RobotStatus, bool)
	Tasks    func() (inputs.TaskSummary, bool)
	Vertices VertexSource
	Log      *slog.Logger

	mu  sync.Mutex
	out io.Writer // nil = records are only logged

	// The stops of the loaded map; touched by Decide only, which the runtime
	// never runs twice at once.
	locMap string
	locs   []string
	locAt  time.Time
}

// SetOutput sets where records are written, one JSON object per line.
func (s *Shadow) SetOutput(w io.Writer) {
	s.mu.Lock()
	s.out = w
	s.mu.Unlock()
}

// Decide has the signature of runtime.Options.Decide.
func (s *Shadow) Decide(ctx context.Context, t runtime.Tick) {
	st := s.state(ctx)
	rec := Record{Time: t.At, Tick: t.N, Trigger: t.Trigger, Reasons: t.Reasons, Model: s.Model, Prompt: t.Prompt, State: st}

	resp, err := s.LLM.Complete(ctx, Request(t.Prompt, st.Locations))
	if err != nil {
		if ctx.Err() != nil {
			return // shutting down
		}
		rec.Error = err.Error()
		s.Log.Error("llm call failed", "tick", t.N, "err", err)
	} else {
		rec.Fill(resp, st)
		for _, c := range rec.Calls {
			s.Log.Info("shadow decision", "tick", t.N, "tool", c.Name, "arguments", c.Arguments,
				"allowed", c.Allowed, "refusal", c.Refusal, "latency", resp.Latency.Round(time.Millisecond))
		}
		if len(rec.Calls) == 0 {
			s.Log.Warn("model made no tool call", "tick", t.N, "content", resp.Content)
		}
	}
	s.write(rec)
}

// Request builds the completion for a prompt: the fused prompt as the one user
// message, with the tools for the given stops.
func Request(prompt string, locations []string) llm.Request {
	return llm.Request{
		Messages: []llm.Message{{Role: "user", Content: prompt}},
		Tools:    tools.Definitions(locations),
	}
}

// Fill records a response and judges each of its calls against st.
func (r *Record) Fill(resp llm.Response, st tools.State) {
	r.Content = resp.Content
	r.LatencyMS = resp.Latency.Milliseconds()
	r.PromptTokens, r.CompletionTokens, r.TokensPerSecond = resp.PromptTokens, resp.CompletionTokens, resp.TokensPerSecond
	r.Calls = nil
	for _, tc := range resp.ToolCalls {
		c := Call{Name: tc.Name, Arguments: string(tc.Arguments), Allowed: true}
		if err := tools.Validate(tc, st); err != nil {
			c.Allowed, c.Refusal, c.Malformed = false, err.Error(), errors.Is(err, tools.ErrMalformed)
		}
		r.Calls = append(r.Calls, c)
	}
}

// state gathers the typed state the calls of this tick are judged against.
func (s *Shadow) state(ctx context.Context) tools.State {
	var st tools.State
	st.Robot, st.RobotOK = s.Robot()
	st.Tasks, st.TasksOK = s.Tasks()
	if st.RobotOK {
		st.Locations = s.locations(ctx, st.Robot.Map)
	}
	return st
}

// locations returns the stops of the map, reading them again when the map
// changes or they have aged. On a failed read the old list of the same map is
// kept; another map's is never used.
func (s *Shadow) locations(ctx context.Context, mapName string) []string {
	if mapName == "" {
		return nil
	}
	if mapName == s.locMap && time.Since(s.locAt) < locationsTTL {
		return s.locs
	}
	vs, err := s.Vertices.GetVertices(ctx, mapName)
	if err != nil {
		s.Log.Warn("cannot read the map's stops", "map", mapName, "err", err)
		if mapName != s.locMap {
			s.locMap, s.locs = mapName, nil
			s.locAt = time.Time{} // try again on the next decision
		}
		return s.locs
	}
	var names []string
	for _, v := range vs {
		if v.Type == backend.VertexMove && v.Name != "" {
			names = append(names, v.Name)
		}
	}
	slices.Sort(names)
	s.locMap, s.locs, s.locAt = mapName, names, time.Now()
	return names
}

func (s *Shadow) write(rec Record) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.out == nil {
		return
	}
	line, err := json.Marshal(rec)
	if err == nil {
		_, err = s.out.Write(append(line, '\n'))
	}
	if err != nil {
		s.Log.Error("cannot write the shadow record", "err", err)
	}
}
