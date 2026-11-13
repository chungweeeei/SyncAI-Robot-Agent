package shadow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"testing"
	"time"

	"github.com/chungweeeei/SyncAI-Robot-Agent/internal/backend"
	"github.com/chungweeeei/SyncAI-Robot-Agent/internal/inputs"
	"github.com/chungweeeei/SyncAI-Robot-Agent/internal/llm"
	"github.com/chungweeeei/SyncAI-Robot-Agent/internal/runtime"
)

type fakeLLM struct {
	resp llm.Response
	err  error
	req  llm.Request
}

func (f *fakeLLM) Complete(_ context.Context, req llm.Request) (llm.Response, error) {
	f.req = req
	return f.resp, f.err
}

type fakeVertices struct {
	calls int
	err   error
}

func (f *fakeVertices) GetVertices(_ context.Context, mapName string) ([]backend.Vertex, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return []backend.Vertex{
		{Name: "lobby", Type: "MOVE"}, {Name: "charger", Type: "CHARGE"}, {Name: "door", Type: "MOVE"},
	}, nil
}

func call(name, args string) llm.ToolCall {
	return llm.ToolCall{Name: name, Arguments: json.RawMessage(args)}
}

func newShadow(m *fakeLLM, v *fakeVertices, locked bool) (*Shadow, *bytes.Buffer) {
	s := &Shadow{
		LLM: m, Model: "test-model", Vertices: v,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Robot: func() (inputs.RobotStatus, bool) {
			return inputs.RobotStatus{Mode: "AUTO", Motion: "STAND", PoseValid: true, Map: "dp", MotorsSeen: true, SafetyLocked: locked}, true
		},
		Tasks: func() (inputs.TaskSummary, bool) { return inputs.TaskSummary{}, true },
	}
	var out bytes.Buffer
	s.SetOutput(&out)
	return s, &out
}

func decode(t *testing.T, out *bytes.Buffer) Record {
	t.Helper()
	var rec Record
	if err := json.Unmarshal(out.Bytes(), &rec); err != nil {
		t.Fatalf("record is not one JSON line: %v\n%s", err, out.String())
	}
	return rec
}

func TestDecideRecordsAndJudges(t *testing.T) {
	m := &fakeLLM{resp: llm.Response{
		ToolCalls: []llm.ToolCall{
			call("create_task", `{"steps": [{"type": "MOVE", "location": "door"}]}`),
			call("create_task", `{"steps": [{"type": "MOVE", "location": "charger"}]}`),
			call("report", `{"message": `), // not JSON; must still be recorded
		},
		Latency: 1500 * time.Millisecond, PromptTokens: 200, CompletionTokens: 30, TokensPerSecond: 25,
	}}
	v := &fakeVertices{}
	s, out := newShadow(m, v, false)
	s.Decide(context.Background(), runtime.Tick{N: 7, Trigger: "wake", Reasons: []string{"task_status:task_failed"}, Prompt: "PROMPT"})

	rec := decode(t, out)
	if rec.Tick != 7 || rec.Trigger != "wake" || rec.Model != "test-model" || rec.Prompt != "PROMPT" ||
		rec.LatencyMS != 1500 || rec.TokensPerSecond != 25 || rec.Error != "" {
		t.Errorf("record = %+v", rec)
	}
	// A charger is not a stop the agent may walk to.
	if !reflect.DeepEqual(rec.State.Locations, []string{"door", "lobby"}) {
		t.Errorf("locations = %v", rec.State.Locations)
	}
	if len(rec.Calls) != 3 || !rec.Calls[0].Allowed || rec.Calls[1].Allowed || rec.Calls[2].Allowed ||
		rec.Calls[1].Refusal == "" || rec.Calls[1].Malformed || !rec.Calls[2].Malformed || rec.Calls[2].Arguments != `{"message": ` {
		t.Errorf("calls = %+v", rec.Calls)
	}

	// The model is given the prompt and a schema naming exactly those stops.
	if len(m.req.Messages) != 1 || m.req.Messages[0].Content != "PROMPT" || len(m.req.Tools) != 6 {
		t.Errorf("request = %+v", m.req)
	}
	schema, _ := json.Marshal(m.req.Tools[0].Parameters)
	if !bytes.Contains(schema, []byte(`"enum":["door","lobby"]`)) {
		t.Errorf("schema does not carry the stops: %s", schema)
	}

	// The stops are read once per map, not once per decision.
	s.Decide(context.Background(), runtime.Tick{N: 8})
	if v.calls != 1 {
		t.Errorf("vertices read %d times, want 1", v.calls)
	}
}

func TestDecideUnderSafetyLock(t *testing.T) {
	m := &fakeLLM{resp: llm.Response{ToolCalls: []llm.ToolCall{call("create_task", `{"steps": [{"type": "STANDUP"}]}`)}}}
	s, out := newShadow(m, &fakeVertices{}, true)
	s.Decide(context.Background(), runtime.Tick{N: 1})
	if rec := decode(t, out); len(rec.Calls) != 1 || rec.Calls[0].Allowed || !rec.State.Robot.SafetyLocked {
		t.Errorf("record = %+v", rec)
	}
}

func TestDecideRecordsFailure(t *testing.T) {
	m := &fakeLLM{err: errors.New("connection refused")}
	s, out := newShadow(m, &fakeVertices{err: errors.New("502")}, false)
	s.Decide(context.Background(), runtime.Tick{N: 1, Prompt: "PROMPT"})
	rec := decode(t, out)
	if rec.Error != "connection refused" || len(rec.Calls) != 0 || rec.Prompt != "PROMPT" || rec.State.Locations != nil {
		t.Errorf("record = %+v", rec)
	}
}

func TestDecideWithoutOutput(t *testing.T) {
	s, _ := newShadow(&fakeLLM{}, &fakeVertices{}, false)
	s.SetOutput(nil)
	s.Decide(context.Background(), runtime.Tick{N: 1}) // must not panic
}
