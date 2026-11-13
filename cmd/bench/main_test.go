package main

import (
	"os"
	"strings"
	"testing"

	"github.com/chungweeeei/SyncAI-Robot-Agent/internal/config"
	"github.com/chungweeeei/SyncAI-Robot-Agent/internal/llm"
	"github.com/chungweeeei/SyncAI-Robot-Agent/internal/shadow"
	"github.com/chungweeeei/SyncAI-Robot-Agent/internal/tools"
)

func res(expect string, latency int64, calls ...shadow.Call) result {
	var want []string
	if expect != "" {
		want = strings.Split(expect, "|")
	}
	return result{Item: item{Expect: want}, Record: shadow.Record{Calls: calls, LatencyMS: latency, TokensPerSecond: 20}}
}

func TestSummarize(t *testing.T) {
	ok := shadow.Call{Name: "report", Allowed: true}
	refused := shadow.Call{Name: "create_task", Refusal: "a task is already active"}
	malformed := shadow.Call{Name: "fly", Refusal: "unknown tool", Malformed: true}

	got := summarize([]result{
		res("report", 100, ok),                // good
		res("do_nothing|pause_task", 200, ok), // passes the guard, but not an expected tool
		res("", 300, refused),                 // well-formed, refused
		res("report", 400, ok, malformed),     // one bad call spoils the answer
		res("", 500),                          // no call at all
		{Record: shadow.Record{Error: "x"}},   // the completion failed
	})
	want := `  asked              6 (1 failed to complete)
  well-formed calls  3/6 (50%)
  passed the guard   2/6 (33%)
  expected tool      1/3 (33%)
  latency            p50 300 ms, p95 500 ms
  decode rate        20.0 tok/s (mean)
`
	if got != want {
		t.Errorf("\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestPercentile(t *testing.T) {
	v := []int64{10, 20, 30, 40}
	if p50, p95, one := percentile(v, 50), percentile(v, 95), percentile(v[:1], 95); p50 != 20 || p95 != 40 || one != 10 {
		t.Errorf("p50 = %d, p95 = %d, single = %d", p50, p95, one)
	}
}

func TestItemsFromLog(t *testing.T) {
	log := `{"tick": 3, "reasons": ["task_status:task_failed"], "prompt": "P1", "state": {"RobotOK": true, "Locations": ["lobby"]}}

{"tick": 4, "prompt": "P2", "error": "timeout", "state": {}}
`
	items, err := itemsFromLog(strings.NewReader(log))
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].Name != "tick 3 [task_status:task_failed]" || items[0].Prompt != "P1" ||
		!items[0].State.RobotOK || items[0].State.Locations[0] != "lobby" || items[1].Prompt != "P2" {
		t.Errorf("items = %+v", items)
	}
	if _, err := itemsFromLog(strings.NewReader("{not json")); err == nil || !strings.Contains(err.Error(), "line 1") {
		t.Errorf("err = %v, want the line number", err)
	}
}

// The shipped cases must load, and each one's state must make its expected
// tool possible: a case the guard can never pass would score every model down.
func TestShippedCases(t *testing.T) {
	cfg, err := config.Load("../../config/dog.json")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("../../config/bench_cases.json")
	if err != nil {
		t.Fatal(err)
	}
	items, err := itemsFromCases(raw, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) < 8 {
		t.Errorf("only %d cases", len(items))
	}
	sample := map[string]string{
		tools.CreateTask: `{"steps": [{"type": "MOVE", "location": "lobby"}]}`,
		tools.PauseTask:  `{}`,
		tools.ResumeTask: `{}`,
		tools.CancelTask: `{}`,
		tools.Report:     `{"message": "x"}`,
		tools.DoNothing:  `{}`,
	}
	for _, it := range items {
		if len(it.Expect) == 0 {
			t.Errorf("%s: expects nothing", it.Name)
		}
		for _, tool := range it.Expect {
			args, known := sample[tool]
			if !known {
				t.Errorf("%s: expects %q, which is not a tool", it.Name, tool)
				continue
			}
			if err := tools.Validate(llm.ToolCall{Name: tool, Arguments: []byte(args)}, it.State); err != nil {
				t.Errorf("%s: the expected %s can never pass the guard: %v", it.Name, tool, err)
			}
		}
		if !strings.Contains(it.Prompt, "Available actions:") || !strings.Contains(it.Prompt, "Current observations:") {
			t.Errorf("%s: prompt is missing a section:\n%s", it.Name, it.Prompt)
		}
	}
}
