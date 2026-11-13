package inputs

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chungweeeei/SyncAI-Robot-Agent/internal/backend"
)

// asOf is far from the local clock on purpose: an Age that comes out right
// proves it was measured against the backend's as_of.
var asOf = time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)

// fakeTasks is a TaskStatusSource whose answers a test sets directly.
type fakeTasks struct {
	mu      sync.Mutex
	list    []backend.ActiveTask
	listErr error
	states  map[string]backend.TaskState
	errs    map[string][]error // per id, consumed one per lookup
}

func newFakeTasks() *fakeTasks {
	return &fakeTasks{states: map[string]backend.TaskState{}, errs: map[string][]error{}}
}

func (f *fakeTasks) GetActiveTasks(context.Context) (backend.ActiveTasks, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return backend.ActiveTasks{}, f.listErr
	}
	return backend.ActiveTasks{Tasks: slices.Clone(f.list), AsOf: asOf}, nil
}

func (f *fakeTasks) GetTaskState(_ context.Context, id string) (backend.TaskState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if q := f.errs[id]; len(q) > 0 {
		f.errs[id] = q[1:]
		return backend.TaskState{}, q[0]
	}
	st, ok := f.states[id]
	if !ok {
		return backend.TaskState{}, backend.ErrNotFound
	}
	return st, nil
}

// set lists a task (or not) and sets what GET /tasks/{id} answers for it.
func (f *fakeTasks) set(listed bool, t backend.ActiveTask, status string, steps ...backend.StepState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.list = slices.DeleteFunc(f.list, func(x backend.ActiveTask) bool { return x.ID == t.ID })
	if listed {
		f.list = append(f.list, t)
	}
	f.states[t.ID] = backend.TaskState{ID: t.ID, Status: status, Steps: steps}
}

func patrol() backend.ActiveTask {
	return backend.ActiveTask{ID: "robot01-goal-1", Status: backend.TaskInProgress,
		StartedAt: asOf.Add(-45 * time.Second), Source: "DIRECT", Kind: "goal", Name: "patrol-A"}
}

func steps(statuses ...string) []backend.StepState {
	ids := []string{"goto_lobby", "goto_door", "goto_dock"}
	out := make([]backend.StepState, len(statuses))
	for i, st := range statuses {
		out[i] = backend.StepState{ID: ids[i], Status: st}
	}
	return out
}

// newPolled returns a sensor that is already online, so that later polls wake
// per transition.
func newPolled(t *testing.T, f *fakeTasks) (*TaskStatus, *wakeLog) {
	t.Helper()
	s := NewTaskStatus(f, DefaultTaskStatusConfig())
	w := &wakeLog{}
	s.poll(context.Background(), w.wake)
	if got := w.take(); !slices.Equal(got, []string{"task_status:online"}) {
		t.Fatalf("first poll woke %v, want only online", got)
	}
	return s, w
}

// take returns the reasons logged so far and clears them.
func (w *wakeLog) take() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	r := w.reasons
	w.reasons = nil
	return r
}

func pollWant(t *testing.T, s *TaskStatus, w *wakeLog, wantWakes []string, wantText string) {
	t.Helper()
	s.poll(context.Background(), w.wake)
	for i := range wantWakes {
		wantWakes[i] = "task_status:" + wantWakes[i]
	}
	if got := w.take(); !slices.Equal(got, wantWakes) {
		t.Errorf("wakes = %v, want %v", got, wantWakes)
	}
	if got := s.Snapshot().Text; wantText != "" && got != wantText {
		t.Errorf("\ngot  %s\nwant %s", got, wantText)
	}
}

func TestTaskLifecycle(t *testing.T) {
	f := newFakeTasks()
	s, w := newPolled(t, f)
	pollWant(t, s, w, nil, "no active task.")

	f.set(true, patrol(), backend.TaskInProgress, steps("COMPLETED", "IN_PROGRESS", "PENDING")...)
	pollWant(t, s, w, []string{"task_started"},
		`task "patrol-A" (goal, started 45s ago) IN_PROGRESS at step 2/3 (goto_door) — robot is busy; do not issue new tasks.`)

	// A step advancing changes the text and wakes nothing.
	f.set(true, patrol(), backend.TaskInProgress, steps("COMPLETED", "COMPLETED", "IN_PROGRESS")...)
	pollWant(t, s, w, nil,
		`task "patrol-A" (goal, started 45s ago) IN_PROGRESS at step 3/3 (goto_dock) — robot is busy; do not issue new tasks.`)

	// The list keeps saying IN_PROGRESS; the hold is only in the live state.
	f.set(true, patrol(), backend.TaskPaused, steps("COMPLETED", "COMPLETED", "PAUSED")...)
	pollWant(t, s, w, []string{"task_paused"},
		`task "patrol-A" (goal, started 45s ago) PAUSED at step 3/3 (goto_dock) — robot is holding; do not issue new tasks; it must be resumed or canceled first.`)

	// A failed lookup keeps the hold instead of reporting a resume.
	f.errs[patrol().ID] = []error{errors.New("502")}
	pollWant(t, s, w, nil, "")
	if sum, _ := s.Current(); len(sum.Active) != 1 || sum.Active[0].Status != backend.TaskPaused {
		t.Errorf("after a failed lookup: %+v, want the task still PAUSED", sum.Active)
	}

	f.set(true, patrol(), backend.TaskInProgress, steps("COMPLETED", "COMPLETED", "IN_PROGRESS")...)
	pollWant(t, s, w, []string{"task_resumed"}, "")

	f.set(false, patrol(), backend.TaskCompleted, steps("COMPLETED", "COMPLETED", "COMPLETED")...)
	pollWant(t, s, w, []string{"task_completed"}, `no active task. Last task "patrol-A" (goal) COMPLETED 0s ago.`)
	if sum, ok := s.Current(); !ok || sum.Busy() || sum.Last == nil {
		t.Errorf("Current = %+v, %v; want idle with a last task", sum, ok)
	}
}

func TestTaskEndings(t *testing.T) {
	long := "nav   timeout:\n" + strings.Repeat("x", 300)
	tests := []struct {
		name     string
		status   string
		steps    []backend.StepState
		wake     string
		wantText string
	}{
		{"failed", backend.TaskFailed,
			[]backend.StepState{{ID: "goto_lobby", Status: "COMPLETED"}, {ID: "goto_door", Status: "FAILED", ErrorMsg: "nav timeout."}, {ID: "goto_dock", Status: "PENDING"}},
			"task_failed", `no active task. Last task "patrol-A" (goal) FAILED 0s ago at step 2/3 (goto_door): nav timeout.`},
		{"failed, long message", backend.TaskFailed,
			[]backend.StepState{{ID: "goto_lobby", Status: "FAILED", ErrorMsg: long}},
			"task_failed", `no active task. Last task "patrol-A" (goal) FAILED 0s ago at step 1/1 (goto_lobby): nav timeout: ` + strings.Repeat("x", 147) + "….",
		},
		{"failed, no steps", backend.TaskFailed, nil,
			"task_failed", `no active task. Last task "patrol-A" (goal) FAILED 0s ago, step detail unavailable.`},
		{"canceled", backend.TaskCanceled,
			[]backend.StepState{{ID: "goto_lobby", Status: "COMPLETED"}, {ID: "goto_door", Status: "CANCELED", ErrorMsg: "Task canceled"}, {ID: "goto_dock", Status: "PENDING"}},
			"task_canceled", `no active task. Last task "patrol-A" (goal) CANCELED 0s ago at step 2/3 (goto_door).`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeTasks()
			s, w := newPolled(t, f)
			f.set(true, patrol(), backend.TaskInProgress, steps("IN_PROGRESS")...)
			pollWant(t, s, w, []string{"task_started"}, "")
			f.set(false, patrol(), tt.status, tt.steps...)
			pollWant(t, s, w, []string{tt.wake}, tt.wantText)
		})
	}
}

func TestTaskFinalLookup(t *testing.T) {
	gone := func(t *testing.T) (*fakeTasks, *TaskStatus, *wakeLog) {
		f := newFakeTasks()
		s, w := newPolled(t, f)
		f.set(true, patrol(), backend.TaskInProgress, steps("IN_PROGRESS")...)
		pollWant(t, s, w, []string{"task_started"}, "")
		f.set(false, patrol(), backend.TaskFailed, backend.StepState{ID: "goto_lobby", Status: "FAILED", ErrorMsg: "blocked"})
		return f, s, w
	}

	t.Run("404", func(t *testing.T) {
		f, s, w := gone(t)
		delete(f.states, patrol().ID)
		pollWant(t, s, w, []string{"task_ended"}, `no active task. Last task "patrol-A" (goal) ended 0s ago, final status unknown.`)
	})
	t.Run("two errors, then the answer", func(t *testing.T) {
		f, s, w := gone(t)
		f.errs[patrol().ID] = []error{errors.New("502"), errors.New("502")}
		pollWant(t, s, w, nil, "")
		pollWant(t, s, w, nil, "")
		if sum, _ := s.Current(); !sum.Busy() {
			t.Error("the task should still count as running while its end is unknown")
		}
		pollWant(t, s, w, []string{"task_failed"}, `no active task. Last task "patrol-A" (goal) FAILED 0s ago at step 1/1 (goto_lobby): blocked.`)
	})
	t.Run("retries used up", func(t *testing.T) {
		f, s, w := gone(t)
		f.errs[patrol().ID] = []error{errors.New("502"), errors.New("502"), errors.New("502")}
		pollWant(t, s, w, nil, "")
		pollWant(t, s, w, nil, "")
		pollWant(t, s, w, []string{"task_ended"}, "")
	})
}

func TestTaskEndedWhileStillListed(t *testing.T) {
	// The cached list still names a task the live state says is over: one
	// task_completed, and no task_started on the next poll.
	f := newFakeTasks()
	s, w := newPolled(t, f)
	f.set(true, patrol(), backend.TaskInProgress, steps("IN_PROGRESS")...)
	pollWant(t, s, w, []string{"task_started"}, "")

	f.set(true, patrol(), backend.TaskCompleted, steps("COMPLETED")...)
	pollWant(t, s, w, []string{"task_completed"}, `no active task. Last task "patrol-A" (goal) COMPLETED 0s ago.`)
	pollWant(t, s, w, nil, "")

	f.set(false, patrol(), backend.TaskCompleted, steps("COMPLETED")...)
	pollWant(t, s, w, nil, "")
	if len(s.ended) != 0 {
		t.Errorf("ended = %v, want it emptied once the list drops the id", s.ended)
	}
}

func TestTwoTasks(t *testing.T) {
	f := newFakeTasks()
	s, w := newPolled(t, f)
	sched := backend.ActiveTask{ID: "robot01-schedule-1", StartedAt: asOf.Add(-5 * time.Minute),
		Source: backend.SourceSchedule, Kind: "schedule", Name: "patrol-A"}
	stand := backend.ActiveTask{ID: "robot01-standup-2", StartedAt: asOf.Add(-10 * time.Second), Source: "DIRECT"}
	f.set(true, stand, backend.TaskInProgress, backend.StepState{ID: "step1", Status: "IN_PROGRESS"})
	f.set(true, sched, backend.TaskInProgress) // no step detail
	pollWant(t, s, w, []string{"task_started", "task_started"},
		`2 active tasks: "patrol-A" (schedule, scheduled, started 5m ago) IN_PROGRESS, step detail unavailable; `+
			`"robot01-standup-2" (started 10s ago) IN_PROGRESS at step 1/1 (step1) — robot is busy; do not issue new tasks.`)
	if sum, _ := s.Current(); len(sum.Active) != 2 || sum.Active[0].ID != sched.ID {
		t.Errorf("Active = %+v, want both, oldest first", sum.Active)
	}
}

func TestTaskStatusStaleAndRecovered(t *testing.T) {
	f := newFakeTasks()
	cfg := DefaultTaskStatusConfig()
	cfg.PollInterval, cfg.StaleAfter = 10*time.Millisecond, 60*time.Millisecond
	s := NewTaskStatus(f, cfg)
	w := &wakeLog{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx, w.wake) }()

	waitFor(t, "online", func() bool { return w.count("task_status:online") == 1 })

	f.mu.Lock()
	f.listErr = errors.New("502")
	f.mu.Unlock()
	waitFor(t, "stale", func() bool { return w.count("task_status:stale") == 1 })
	time.Sleep(3 * cfg.StaleAfter)
	if n := w.count("task_status:stale"); n != 1 {
		t.Errorf("stale woke %d times, want 1", n)
	}
	if _, ok := s.Current(); ok {
		t.Error("Current should not be ok while stale")
	}
	if got := s.Snapshot().Render(s.Name(), time.Now()); !strings.Contains(got, "STALE") {
		t.Errorf("render = %q, want STALE", got)
	}

	// A task that started during the outage is reported by recovered alone.
	f.set(true, patrol(), backend.TaskInProgress, steps("IN_PROGRESS")...)
	f.mu.Lock()
	f.listErr = nil
	f.mu.Unlock()
	waitFor(t, "recovered", func() bool { return w.count("task_status:recovered") == 1 })
	if n := w.count("task_status:task_started"); n != 0 {
		t.Errorf("task_started woke %d times across the outage, want 0", n)
	}

	cancel()
	if err := <-done; err != context.Canceled {
		t.Errorf("Run returned %v, want context.Canceled", err)
	}
}

func TestFmtAge(t *testing.T) {
	for d, want := range map[time.Duration]string{
		-time.Second: "0s", 45 * time.Second: "45s", 119 * time.Second: "1m",
		2 * time.Minute: "2m", 3*time.Hour + time.Minute: "3h",
	} {
		if got := fmtAge(d); got != want {
			t.Errorf("fmtAge(%v) = %q, want %q", d, got, want)
		}
	}
}
