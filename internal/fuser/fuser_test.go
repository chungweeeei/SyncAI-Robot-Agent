package fuser

import (
	"testing"
	"time"
)

var now = time.Date(2006, 1, 2, 15, 4, 0, 0, time.UTC)

// The golden text below is what the agent sees on a tick.
func TestFuse(t *testing.T) {
	f := New(PromptConfig{
		SystemPromptBase:     "  You are a robot dog.\n",
		SystemGovernance:     "Never move when ESTOP.",
		SystemPromptExamples: "Example: when idle, say hello.",
	})
	got := f.Fuse(now, []string{
		`robot_state: mode AUTO; motion STAND; pose (1.0, 2.0, 90°) on map "dp1f"; battery 80%; 12 motors ok.`,
		"",
		"task_status: no active task.",
	})
	want := `You are a robot dog.

Governance rules:
Never move when ESTOP.

Current time is Monday, January 2, 2006 at 3:04 PM.

Current observations:
- robot_state: mode AUTO; motion STAND; pose (1.0, 2.0, 90°) on map "dp1f"; battery 80%; 12 motors ok.
- task_status: no active task.

Example: when idle, say hello.

What will you do next?`
	if got != want {
		t.Errorf("\ngot:\n%s\n\nwant:\n%s", got, want)
	}
}

func TestFuseActions(t *testing.T) {
	f := New(PromptConfig{SystemPromptBase: "You are a robot dog.", Actions: []string{"report: tell the operator", "do_nothing: wait"}})
	want := `You are a robot dog.

Current time is Monday, January 2, 2006 at 3:04 PM.

Current observations:
- task_status: no active task.

Available actions:
- report: tell the operator
- do_nothing: wait

What will you do next?`
	if got := f.Fuse(now, []string{"task_status: no active task."}); got != want {
		t.Errorf("\ngot:\n%s\n\nwant:\n%s", got, want)
	}
}

func TestFuseOmitsEmptySections(t *testing.T) {
	want := `Current time is Monday, January 2, 2006 at 3:04 PM.

What will you do next?`
	if got := New(PromptConfig{}).Fuse(now, nil); got != want {
		t.Errorf("\ngot:\n%s\n\nwant:\n%s", got, want)
	}
}
