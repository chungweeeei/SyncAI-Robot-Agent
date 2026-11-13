package tools

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/chungweeeei/SyncAI-Robot-Agent/internal/backend"
	"github.com/chungweeeei/SyncAI-Robot-Agent/internal/inputs"
	"github.com/chungweeeei/SyncAI-Robot-Agent/internal/llm"
)

// idle is a robot that may be given a job.
func idle() State {
	return State{
		Robot:     inputs.RobotStatus{Mode: "AUTO", Motion: "STAND", Policy: "PPO", PoseValid: true, Map: "dp", Battery: 80, MotorsSeen: true},
		RobotOK:   true,
		TasksOK:   true,
		Locations: []string{"lobby", "door"},
	}
}

func busy(status string) func(*State) {
	return func(s *State) {
		s.Tasks.Active = []inputs.ActiveTaskInfo{{ID: "t1", Status: status}}
	}
}

func TestValidate(t *testing.T) {
	const move = `{"steps": [{"type": "MOVE", "location": "lobby"}]}`
	tests := []struct {
		name   string
		tool   string
		args   string
		mutate func(*State)
		reject string // "" = allowed
	}{
		{"move", CreateTask, move, nil, ""},
		{"patrol then speak", CreateTask, `{"steps": [{"type": "PATROL", "locations": ["lobby", "door"]}, {"type": "SPEAK", "text": "done"}]}`, nil, ""},
		{"posture and wait", CreateTask, `{"steps": [{"type": "LIEDOWN"}, {"type": "WAIT", "seconds": 60}, {"type": "STANDUP"}]}`, nil, ""},

		{"robot state unknown", CreateTask, move, func(s *State) { s.RobotOK = false }, "robot state is unknown"},
		{"manual mode", CreateTask, move, func(s *State) { s.Robot.Mode = "MANUAL" }, "MANUAL mode"},
		{"safety lock", CreateTask, move, func(s *State) { s.Robot.SafetyLocked = true }, "safety lock"},
		{"estop", CreateTask, move, func(s *State) { s.Robot.Motion = inputs.MotionEstop }, "ESTOP"},
		{"damping", CreateTask, `{"steps": [{"type": "SPEAK", "text": "hi"}]}`, func(s *State) { s.Robot.Motion = inputs.MotionDamping }, "DAMPING"},
		{"task state unknown", CreateTask, move, func(s *State) { s.TasksOK = false }, "task state is unknown"},
		{"task running", CreateTask, move, busy(backend.TaskInProgress), "already active"},
		{"task held", CreateTask, move, busy(backend.TaskPaused), "already active"},
		{"not localized", CreateTask, move, func(s *State) { s.Robot.PoseValid = false }, "not localized"},
		{"motion unknown", CreateTask, `{"steps": [{"type": "STANDUP"}]}`, func(s *State) { s.Robot.Motion = inputs.MotionUnknown }, "motion state is unknown"},
		{"speak needs no gait controller", CreateTask, `{"steps": [{"type": "SPEAK", "text": "hi"}]}`, func(s *State) { s.Robot.Motion = inputs.MotionUnknown; s.Robot.PoseValid = false }, ""},

		{"unknown location", CreateTask, `{"steps": [{"type": "MOVE", "location": "roof"}]}`, nil, `unknown location "roof"`},
		{"move without a location", CreateTask, `{"steps": [{"type": "MOVE"}]}`, nil, `unknown location ""`},
		{"coordinates are not a field", CreateTask, `{"steps": [{"type": "MOVE", "x": 1.0, "y": 2.0}]}`, nil, "malformed call"},
		{"patrol with one stop", CreateTask, `{"steps": [{"type": "PATROL", "locations": ["lobby"]}]}`, nil, "at least two"},
		{"patrol with an unknown stop", CreateTask, `{"steps": [{"type": "PATROL", "locations": ["lobby", "roof"]}]}`, nil, "step 1 (PATROL)"},
		{"low-level step type", CreateTask, `{"steps": [{"type": "TELEOP"}]}`, nil, "unknown step type"},
		{"charge is not offered", CreateTask, `{"steps": [{"type": "CHARGE", "location": "lobby"}]}`, nil, "unknown step type"},
		{"empty speech", CreateTask, `{"steps": [{"type": "SPEAK", "text": " "}]}`, nil, "text must be"},
		{"speech too long", CreateTask, `{"steps": [{"type": "SPEAK", "text": "` + strings.Repeat("a", 1001) + `"}]}`, nil, "text must be"},
		{"wait too long", CreateTask, `{"steps": [{"type": "WAIT", "seconds": 3601}]}`, nil, "seconds must be"},
		{"no steps", CreateTask, `{"steps": []}`, nil, "no steps"},
		{"no arguments", CreateTask, ``, nil, "no steps"},
		{"too many steps", CreateTask, `{"steps": [` + strings.Repeat(`{"type": "STANDUP"},`, 8) + `{"type": "STANDUP"}]}`, nil, "at most 8"},
		{"broken json", CreateTask, `{"steps": [{"type": "MOVE", `, nil, "malformed call"},
		{"second step is judged too", CreateTask, `{"steps": [{"type": "STANDUP"}, {"type": "MOVE", "location": "roof"}]}`, nil, "step 2 (MOVE)"},

		{"pause", PauseTask, `{}`, busy(backend.TaskInProgress), ""},
		{"pause, nothing running", PauseTask, `{}`, nil, "no active task"},
		{"pause, already held", PauseTask, `{}`, busy(backend.TaskPaused), "no task is running"},
		{"pause, task state unknown", PauseTask, `{}`, func(s *State) { s.TasksOK = false }, "task state is unknown"},
		{"resume", ResumeTask, ``, busy(backend.TaskPaused), ""},
		{"resume, not held", ResumeTask, `{}`, busy(backend.TaskInProgress), "no task is held"},
		{"cancel", CancelTask, `null`, busy(backend.TaskPaused), ""},
		{"cancel, nothing running", CancelTask, `{}`, nil, "no active task"},
		// Stopping stays possible whatever state the robot is in.
		{"cancel under safety lock", CancelTask, `{}`, func(s *State) { s.Robot.SafetyLocked = true; busy(backend.TaskInProgress)(s) }, ""},

		{"report", Report, `{"message": "door is blocked"}`, func(s *State) { s.RobotOK, s.TasksOK = false, false }, ""},
		{"report, empty", Report, `{"message": ""}`, nil, "message is empty"},
		{"do nothing", DoNothing, `{"reason": "all good"}`, nil, ""},
		{"do nothing, no arguments", DoNothing, ``, nil, ""},
		{"unknown tool", "set_motion_key", `{"key": "1"}`, nil, `unknown tool "set_motion_key"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := idle()
			if tt.mutate != nil {
				tt.mutate(&st)
			}
			err := Validate(llm.ToolCall{Name: tt.tool, Arguments: json.RawMessage(tt.args)}, st)
			switch {
			case tt.reject == "" && err != nil:
				t.Errorf("refused: %v", err)
			case tt.reject != "" && err == nil:
				t.Errorf("allowed, want a refusal mentioning %q", tt.reject)
			case tt.reject != "" && !strings.Contains(err.Error(), tt.reject):
				t.Errorf("refused with %q, want it to mention %q", err, tt.reject)
			}
		})
	}
}

// The schema is what a constrained decoder is held to, so its shape is pinned.
func TestDefinitions(t *testing.T) {
	defs := Definitions([]string{"lobby", "door"})
	got, err := json.Marshal(defs[0].Parameters)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"properties":{"steps":{"items":{"properties":{` +
		`"location":{"description":"MOVE only: the stop to walk to.","enum":["lobby","door"],"type":"string"},` +
		`"locations":{"description":"PATROL only: the stops to visit, in order.","items":{"enum":["lobby","door"],"type":"string"},"minItems":2,"type":"array"},` +
		`"seconds":{"description":"WAIT only: how long to wait.","maximum":3600,"minimum":1,"type":"integer"},` +
		`"text":{"description":"SPEAK only: the English sentence to say.","maxLength":1000,"type":"string"},` +
		`"type":{"enum":["MOVE","PATROL","SPEAK","WAIT","STANDUP","LIEDOWN"],"type":"string"}},` +
		`"required":["type"],"type":"object"},"maxItems":8,"minItems":1,"type":"array"}},"required":["steps"],"type":"object"}`
	if string(got) != want {
		t.Errorf("\ngot  %s\nwant %s", got, want)
	}

	var names []string
	for _, d := range defs {
		names = append(names, d.Name)
	}
	if got := strings.Join(names, " "); got != "create_task pause_task resume_task cancel_task report do_nothing" {
		t.Errorf("tools = %s", got)
	}
}

func TestDefinitionsWithoutLocations(t *testing.T) {
	got, _ := json.Marshal(Definitions(nil)[0].Parameters)
	for _, absent := range []string{"MOVE", "PATROL", "location"} {
		if strings.Contains(string(got), absent) {
			t.Errorf("schema with no locations still mentions %q: %s", absent, got)
		}
	}
}

func TestActions(t *testing.T) {
	got := Actions(Definitions(nil))
	if len(got) != 6 || got[4] != "report: Tell the operator something they need to know: a failure and its likely cause, a risk, a suggestion." {
		t.Errorf("actions = %q", got)
	}
}
