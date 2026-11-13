// Package tools defines what the model may ask for, and judges each request
// against the robot's typed state.
//
// Every tool is a high-level one: a job for the backend's task queue, or a
// message. None carries a velocity, a gait key or a joint target, so the model
// cannot express low-level control at all.
package tools

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/chungweeeei/SyncAI-Robot-Agent/internal/backend"
	"github.com/chungweeeei/SyncAI-Robot-Agent/internal/inputs"
	"github.com/chungweeeei/SyncAI-Robot-Agent/internal/llm"
)

// Tool names.
const (
	CreateTask = "create_task"
	PauseTask  = "pause_task"
	ResumeTask = "resume_task"
	CancelTask = "cancel_task"
	Report     = "report"
	DoNothing  = "do_nothing"
)

// Step types of create_task; the backend's StepType values the agent may use.
const (
	StepMove    = "MOVE"
	StepPatrol  = "PATROL"
	StepSpeak   = "SPEAK"
	StepWait    = "WAIT"
	StepStandUp = "STANDUP"
	StepLieDown = "LIEDOWN"
)

// The backend's own limits (SpeakParams, WaitParams) and the agent's on a job.
const (
	maxSteps       = 8
	maxSpeakChars  = 1000
	maxWaitSeconds = 3600
)

// ErrMalformed marks a call that is not one of the tools or whose arguments do
// not fit the schema, as opposed to a well-formed call the state forbids.
var ErrMalformed = errors.New("malformed call")

// State is everything a call is judged against. It is plain data so that a
// shadow record can carry it and a benchmark can judge the same call again.
type State struct {
	Robot   inputs.RobotStatus
	RobotOK bool // false = robot state unknown or stale
	Tasks   inputs.TaskSummary
	TasksOK bool // false = task state unknown or stale

	// Locations are the names of the stops on the loaded map. A MOVE or PATROL
	// may name only these.
	Locations []string
}

// Definitions returns the tools offered to the model. Location names go into
// the schema as an enum, so a constrained decoder cannot write any other place
// (and none can write coordinates); with no locations MOVE and PATROL are not
// offered at all.
func Definitions(locations []string) []llm.Tool {
	stepTypes := []string{StepSpeak, StepWait, StepStandUp, StepLieDown}
	step := map[string]any{
		"type": map[string]any{"type": "string"},
		"text": map[string]any{
			"type": "string", "maxLength": maxSpeakChars,
			"description": "SPEAK only: the English sentence to say.",
		},
		"seconds": map[string]any{
			"type": "integer", "minimum": 1, "maximum": maxWaitSeconds,
			"description": "WAIT only: how long to wait.",
		},
	}
	if len(locations) > 0 {
		stepTypes = append([]string{StepMove, StepPatrol}, stepTypes...)
		step["location"] = map[string]any{
			"type": "string", "enum": locations,
			"description": "MOVE only: the stop to walk to.",
		}
		step["locations"] = map[string]any{
			"type": "array", "minItems": 2, "items": map[string]any{"type": "string", "enum": locations},
			"description": "PATROL only: the stops to visit, in order.",
		}
	}
	step["type"].(map[string]any)["enum"] = stepTypes

	none := map[string]any{"type": "object", "properties": map[string]any{}}
	return []llm.Tool{
		{
			Name:        CreateTask,
			Description: "Start a new job made of steps run in order. Only when no task is active and the robot is able to act.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"steps": map[string]any{
						"type": "array", "minItems": 1, "maxItems": maxSteps,
						"items": map[string]any{"type": "object", "properties": step, "required": []string{"type"}},
					},
				},
				"required": []string{"steps"},
			},
		},
		{Name: PauseTask, Description: "Hold the running task where it is.", Parameters: none},
		{Name: ResumeTask, Description: "Release a held task.", Parameters: none},
		{Name: CancelTask, Description: "Cancel the active task.", Parameters: none},
		{
			Name:        Report,
			Description: "Tell the operator something they need to know: a failure and its likely cause, a risk, a suggestion.",
			Parameters: map[string]any{
				"type":       "object",
				"properties": map[string]any{"message": map[string]any{"type": "string"}},
				"required":   []string{"message"},
			},
		},
		{
			Name:        DoNothing,
			Description: "Nothing needs doing right now.",
			Parameters: map[string]any{
				"type":       "object",
				"properties": map[string]any{"reason": map[string]any{"type": "string"}},
			},
		},
	}
}

// Actions renders the tools as the lines of the prompt's "Available actions".
func Actions(defs []llm.Tool) []string {
	out := make([]string, len(defs))
	for i, d := range defs {
		out[i] = d.Name + ": " + d.Description
	}
	return out
}

type step struct {
	Type      string   `json:"type"`
	Location  string   `json:"location"`
	Locations []string `json:"locations"`
	Text      string   `json:"text"`
	Seconds   int      `json:"seconds"`
}

// Validate judges one call against the typed state and returns why it is
// refused, or nil. It is the guard: the model's output is untrusted input, the
// prompt's rules are advice, and only what passes here may ever be executed.
// It never reads prompt text.
func Validate(call llm.ToolCall, st State) error {
	switch call.Name {
	case CreateTask:
		var args struct {
			Steps []step `json:"steps"`
		}
		if err := decode(call.Arguments, &args); err != nil {
			return err
		}
		return validateTask(args.Steps, st)

	case PauseTask, ResumeTask, CancelTask:
		if err := decode(call.Arguments, &struct{}{}); err != nil {
			return err
		}
		if !st.TasksOK {
			return errors.New("task state is unknown")
		}
		switch {
		case !st.Tasks.Busy():
			return errors.New("no active task")
		case call.Name == PauseTask && !hasStatus(st.Tasks, backend.TaskInProgress):
			return errors.New("no task is running")
		case call.Name == ResumeTask && !hasStatus(st.Tasks, backend.TaskPaused):
			return errors.New("no task is held")
		}
		return nil

	case Report:
		var args struct {
			Message string `json:"message"`
		}
		if err := decode(call.Arguments, &args); err != nil {
			return err
		}
		if strings.TrimSpace(args.Message) == "" {
			return errors.New("message is empty")
		}
		return nil

	case DoNothing:
		var args struct {
			Reason string `json:"reason"`
		}
		return decode(call.Arguments, &args)

	default:
		return fmt.Errorf("%w: unknown tool %q", ErrMalformed, call.Name)
	}
}

func validateTask(steps []step, st State) error {
	switch {
	case !st.RobotOK:
		return errors.New("robot state is unknown")
	case st.Robot.Mode != inputs.ModeAuto:
		return fmt.Errorf("robot is in %s mode, not AUTO", st.Robot.Mode)
	case st.Robot.SafetyLocked:
		return errors.New("safety lock is engaged")
	case st.Robot.Motion == inputs.MotionEstop || st.Robot.Motion == inputs.MotionDamping:
		return fmt.Errorf("robot is in %s", st.Robot.Motion)
	case !st.TasksOK:
		return errors.New("task state is unknown")
	case st.Tasks.Busy():
		return errors.New("a task is already active")
	case len(steps) == 0:
		return errors.New("no steps")
	case len(steps) > maxSteps:
		return fmt.Errorf("%d steps, at most %d", len(steps), maxSteps)
	}

	for i, s := range steps {
		if err := validateStep(s, st); err != nil {
			return fmt.Errorf("step %d (%s): %w", i+1, s.Type, err)
		}
	}
	return nil
}

func validateStep(s step, st State) error {
	// A step that moves the body needs a gait controller that is reporting.
	body := func() error {
		if st.Robot.Motion == inputs.MotionUnknown {
			return errors.New("motion state is unknown")
		}
		return nil
	}
	// One that walks somewhere needs a pose and a stop that exists.
	walk := func(names ...string) error {
		if err := body(); err != nil {
			return err
		}
		if !st.Robot.PoseValid {
			return errors.New("robot is not localized")
		}
		for _, n := range names {
			if !slices.Contains(st.Locations, n) {
				return fmt.Errorf("unknown location %q", n)
			}
		}
		return nil
	}

	switch s.Type {
	case StepMove:
		return walk(s.Location)
	case StepPatrol:
		if len(s.Locations) < 2 {
			return errors.New("needs at least two locations")
		}
		return walk(s.Locations...)
	case StepStandUp, StepLieDown:
		return body()
	case StepSpeak:
		if n := utf8.RuneCountInString(strings.TrimSpace(s.Text)); n == 0 || n > maxSpeakChars {
			return fmt.Errorf("text must be 1 to %d characters", maxSpeakChars)
		}
		return nil
	case StepWait:
		if s.Seconds < 1 || s.Seconds > maxWaitSeconds {
			return fmt.Errorf("seconds must be 1 to %d", maxWaitSeconds)
		}
		return nil
	default:
		return errors.New("unknown step type")
	}
}

func hasStatus(sum inputs.TaskSummary, status string) bool {
	return slices.ContainsFunc(sum.Active, func(t inputs.ActiveTaskInfo) bool { return t.Status == status })
}

// decode reads a call's arguments strictly: a field the schema does not have is
// a malformed call, not something to guess around. No arguments at all reads as {}.
func decode(raw json.RawMessage, out any) error {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	return nil
}
