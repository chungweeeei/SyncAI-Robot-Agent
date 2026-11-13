package inputs

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/chungweeeei/SyncAI-Robot-Agent/internal/backend"
)

// TaskStatusSource is what TaskStatus reads from; *backend.Client implements it and tests swap in a fake.
type TaskStatusSource interface {
	GetActiveTasks(ctx context.Context) (backend.ActiveTasks, error)
	GetTaskState(ctx context.Context, id string) (backend.TaskState, error)
}

// TaskStatusConfig configures the task_status sensor.
type TaskStatusConfig struct {
	PollInterval time.Duration // the backend caches the list for 1.5 s, so polling faster brings nothing new
	StaleAfter   time.Duration // the snapshot is stale after this long without an update

	// FinalLookupRetries is how many times the final state of a task that left the
	// list is looked up. A Temporal hiccup must not lose "FAILED at step 2: nav timeout".
	FinalLookupRetries int

	ErrorMsgMaxLen int // an error_msg is cut to this length before it enters the prompt
}

// DefaultTaskStatusConfig returns the Phase 1 defaults.
func DefaultTaskStatusConfig() TaskStatusConfig {
	return TaskStatusConfig{
		PollInterval:       2 * time.Second,
		StaleAfter:         10 * time.Second,
		FinalLookupRetries: 3,
		ErrorMsgMaxLen:     160,
	}
}

// ActiveTaskInfo is the typed state of one running task.
type ActiveTaskInfo struct {
	ID, Kind, Name, Source string
	Status                 string // IN_PROGRESS or PAUSED
	StartedAt              time.Time
	Age                    time.Duration // measured against the backend's as_of
	StepIdx, StepCount     int           // StepIdx is 1-based
	StepID                 string
	StepsKnown             bool // false = no step detail available
}

// FinishedTask is the task that ended most recently.
type FinishedTask struct {
	ID, Kind, Name     string
	Status             string // COMPLETED / FAILED / CANCELED; "" = final state unknown
	FinishedAt         time.Time
	Age                time.Duration
	StepIdx, StepCount int
	StepID, ErrorMsg   string
	StepsKnown         bool
}

// TaskSummary is the typed state a guard decides on; the LLM reads the text formatTasks makes of it.
type TaskSummary struct {
	AsOf   time.Time
	Active []ActiveTaskInfo // sorted by StartedAt
	Last   *FinishedTask
}

// Busy reports whether a task is running, a held one included.
func (t TaskSummary) Busy() bool { return len(t.Active) > 0 }

// taskView is what the Run goroutine remembers of one active task.
type taskView struct {
	info   ActiveTaskInfo
	misses int // failed lookups of the final state since the task left the list
}

// TaskStatus is the "task_status" sensor: it polls the backend's active_tasks and tasks/{id}.
type TaskStatus struct {
	Latest

	cfg TaskStatusConfig
	src TaskStatusSource

	// Read and written by the Run goroutine only; no lock needed.
	prev   map[string]*taskView // the active tasks of the previous poll
	ended  map[string]struct{}  // ids known to have ended that the cached active_tasks still lists
	last   *FinishedTask
	online bool // whether a poll has ever succeeded
	stale  bool // whether the stale wake has been sent

	// The state a guard reads: written by Run, read by other goroutines.
	curMu sync.RWMutex
	cur   TaskSummary
	curOK bool
}

// NewTaskStatus creates the task_status sensor.
func NewTaskStatus(src TaskStatusSource, cfg TaskStatusConfig) *TaskStatus {
	return &TaskStatus{
		Latest: Latest{StaleAfter: cfg.StaleAfter},
		cfg:    cfg,
		src:    src,
		prev:   map[string]*taskView{},
		ended:  map[string]struct{}{},
	}
}

// Name implements Sensor.
func (s *TaskStatus) Name() string { return "task_status" }

// Current returns the latest state for a guard. ok is false before the first poll
// and while the data is stale; a guard must read that as "unknown, refuse new tasks".
func (s *TaskStatus) Current() (sum TaskSummary, ok bool) {
	s.curMu.RLock()
	sum, ok = s.cur, s.curOK
	s.curMu.RUnlock()
	if !ok || s.Snapshot().Stale(time.Now()) {
		return TaskSummary{}, false
	}
	return sum, true
}

// Run implements Sensor. An HTTP error never ends Run; the data just goes stale.
func (s *TaskStatus) Run(ctx context.Context, wake WakeFunc) error {
	ticker := time.NewTicker(s.cfg.PollInterval)
	defer ticker.Stop()

	for {
		s.poll(ctx, wake)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// poll runs one poll.
func (s *TaskStatus) poll(ctx context.Context, wake WakeFunc) {
	list, err := s.src.GetActiveTasks(ctx)
	if err != nil {
		// No Set, so the data goes stale by itself; wake once at the moment it does.
		if s.online && !s.stale && s.Snapshot().Stale(time.Now()) {
			s.stale = true
			wake("task_status:stale")
		}
		return
	}

	var reasons []string
	cur := make(map[string]*taskView, len(list.Tasks))
	listed := make(map[string]struct{}, len(list.Tasks))

	for _, t := range list.Tasks {
		listed[t.ID] = struct{}{}
		if _, done := s.ended[t.ID]; done {
			continue
		}
		v := &taskView{info: ActiveTaskInfo{
			ID: t.ID, Kind: t.Kind, Name: t.Name, Source: t.Source,
			Status: t.Status, StartedAt: t.StartedAt,
		}}
		st, err := s.src.GetTaskState(ctx, t.ID)
		switch {
		case err != nil:
			// One failed lookup does not fail the poll. The list never shows PAUSED,
			// so keep the previous status rather than report a false task_resumed.
			if old, ok := s.prev[t.ID]; ok {
				v.info.Status = old.info.Status
			}
		case terminal(st.Status):
			// The list is cached for 1.5 s, so the live GET can know of the end first.
			// Remember the id, or the next poll would take it for a new task.
			reasons = append(reasons, s.finish(v.info, &st, list.AsOf))
			s.ended[t.ID] = struct{}{}
			continue
		default:
			v.apply(st)
		}
		cur[t.ID] = v
	}

	for id := range s.ended {
		if _, ok := listed[id]; !ok {
			delete(s.ended, id)
		}
	}

	// Tasks of the previous poll the list no longer has: look up how they ended.
	for id, old := range s.prev {
		if _, ok := cur[id]; ok {
			continue
		}
		if _, ok := s.ended[id]; ok {
			continue
		}
		st, err := s.src.GetTaskState(ctx, id)
		switch {
		case err == nil && terminal(st.Status):
			reasons = append(reasons, s.finish(old.info, &st, list.AsOf))
		case err == nil:
			// Not terminal yet: visibility lag. Treat it as still running.
			old.apply(st)
			cur[id] = old
		case errors.Is(err, backend.ErrNotFound):
			reasons = append(reasons, s.finish(old.info, nil, list.AsOf))
		default:
			old.misses++
			if old.misses >= s.cfg.FinalLookupRetries {
				reasons = append(reasons, s.finish(old.info, nil, list.AsOf))
			} else {
				cur[id] = old
			}
		}
	}

	reasons = append(diff(s.prev, cur), reasons...)
	s.prev = cur

	sum := TaskSummary{AsOf: list.AsOf}
	for _, v := range cur {
		v.info.Age = list.AsOf.Sub(v.info.StartedAt)
		sum.Active = append(sum.Active, v.info)
	}
	slices.SortFunc(sum.Active, func(a, b ActiveTaskInfo) int {
		if c := a.StartedAt.Compare(b.StartedAt); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	if s.last != nil {
		last := *s.last
		last.Age = list.AsOf.Sub(last.FinishedAt)
		sum.Last = &last
	}

	s.Set(formatTasks(sum))
	s.curMu.Lock()
	s.cur, s.curOK = sum, true
	s.curMu.Unlock()

	switch {
	case !s.online:
		// First poll: the state is all in Text, no wake per transition.
		s.online = true
		wake("task_status:online")
	case s.stale:
		s.stale = false
		wake("task_status:recovered")
	default:
		for _, r := range reasons {
			wake("task_status:" + r)
		}
	}
}

// apply copies the live state of GET /tasks/{id} onto the view.
func (v *taskView) apply(st backend.TaskState) {
	v.misses = 0
	v.info.Status = st.Status
	v.info.StepCount = len(st.Steps)
	v.info.StepsKnown = len(st.Steps) > 0
	v.info.StepIdx, v.info.StepID = currentStep(st.Steps)
}

// finish records the task as the last one that ended and returns the wake reason.
// A nil st means its final state is unknown.
func (s *TaskStatus) finish(info ActiveTaskInfo, st *backend.TaskState, asOf time.Time) string {
	f := &FinishedTask{ID: info.ID, Kind: info.Kind, Name: info.Name, FinishedAt: asOf}
	if st != nil {
		f.Status = st.Status
		f.StepCount = len(st.Steps)
		f.StepsKnown = len(st.Steps) > 0
		var msg string
		f.StepIdx, f.StepID, msg = finalStep(st.Steps, st.Status)
		f.ErrorMsg = sanitize(msg, s.cfg.ErrorMsgMaxLen)
	}
	s.last = f
	switch f.Status {
	case backend.TaskCompleted:
		return "task_completed"
	case backend.TaskFailed:
		return "task_failed"
	case backend.TaskCanceled:
		return "task_canceled"
	default:
		return "task_ended"
	}
}

func terminal(status string) bool {
	return status == backend.TaskCompleted || status == backend.TaskFailed || status == backend.TaskCanceled
}

// diff compares the active tasks of two polls and returns the wake reasons.
// A step advancing wakes nothing: there is nothing for the agent to do mid-task,
// and the base tick picks it up.
func diff(prev, cur map[string]*taskView) []string {
	ids := make([]string, 0, len(cur))
	for id := range cur {
		ids = append(ids, id)
	}
	slices.Sort(ids)

	var r []string
	for _, id := range ids {
		old, ok := prev[id]
		switch {
		case !ok:
			r = append(r, "task_started")
		case old.info.Status != backend.TaskPaused && cur[id].info.Status == backend.TaskPaused:
			r = append(r, "task_paused")
		case old.info.Status == backend.TaskPaused && cur[id].info.Status != backend.TaskPaused:
			r = append(r, "task_resumed")
		}
	}
	return r
}

// currentStep returns the step under way (1-based index and id): the first
// PENDING one when none is, the last one when none is pending either.
func currentStep(steps []backend.StepState) (int, string) {
	pending := -1
	for i, st := range steps {
		switch st.Status {
		case backend.TaskInProgress, backend.TaskPaused:
			return i + 1, st.ID
		case "PENDING":
			if pending < 0 {
				pending = i
			}
		}
	}
	if pending >= 0 {
		return pending + 1, steps[pending].ID
	}
	if n := len(steps); n > 0 {
		return n, steps[n-1].ID
	}
	return 0, ""
}

// finalStep returns the step a task stopped at: the failed one of a FAILED task,
// the interrupted one of a CANCELED task. 0 when there is none, or for COMPLETED.
func finalStep(steps []backend.StepState, status string) (idx int, id, errMsg string) {
	if status == backend.TaskCompleted {
		return 0, "", ""
	}
	for i, st := range steps {
		if st.Status == status {
			if status == backend.TaskFailed {
				errMsg = st.ErrorMsg
			}
			return i + 1, st.ID, errMsg
		}
	}
	return 0, "", ""
}

// sanitize folds an error_msg onto one line and cuts it, so that neither a newline
// nor a very long message breaks the prompt.
func sanitize(msg string, maxLen int) string {
	msg = strings.Join(strings.Fields(msg), " ")
	if r := []rune(msg); maxLen > 0 && len(r) > maxLen {
		msg = string(r[:maxLen]) + "…"
	}
	return msg
}

// fmtAge writes a duration as "45s", "2m" or "3h".
func fmtAge(d time.Duration) string {
	switch {
	case d < time.Second:
		return "0s"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
}

// label is what the prompt calls a task: its name, or its id when it has none.
func label(name, id string) string {
	if name != "" {
		return fmt.Sprintf("%q", name)
	}
	return fmt.Sprintf("%q", id)
}

// formatTasks writes the text the LLM reads. Like formatRobot, every state says
// outright what must not be done. The last task is left out while one runs, to
// save tokens.
func formatTasks(sum TaskSummary) string {
	if len(sum.Active) == 0 {
		if sum.Last == nil {
			return "no active task."
		}
		return "no active task. " + formatLast(*sum.Last)
	}

	parts := make([]string, len(sum.Active))
	held := len(sum.Active) == 1 && sum.Active[0].Status == backend.TaskPaused
	for i, t := range sum.Active {
		parts[i] = formatActive(t)
	}
	tail := " — robot is busy; do not issue new tasks."
	if held {
		tail = " — robot is holding; do not issue new tasks; it must be resumed or canceled first."
	}
	if len(parts) == 1 {
		return "task " + parts[0] + tail
	}
	return fmt.Sprintf("%d active tasks: %s%s", len(parts), strings.Join(parts, "; "), tail)
}

func formatActive(t ActiveTaskInfo) string {
	var tags []string
	if t.Kind != "" {
		tags = append(tags, t.Kind)
	}
	if t.Source == backend.SourceSchedule {
		tags = append(tags, "scheduled")
	}
	tags = append(tags, "started "+fmtAge(t.Age)+" ago")

	s := fmt.Sprintf("%s (%s) %s", label(t.Name, t.ID), strings.Join(tags, ", "), t.Status)
	if !t.StepsKnown {
		return s + ", step detail unavailable"
	}
	return s + fmt.Sprintf(" at step %d/%d (%s)", t.StepIdx, t.StepCount, t.StepID)
}

func formatLast(f FinishedTask) string {
	s := "Last task " + label(f.Name, f.ID)
	if f.Kind != "" {
		s += " (" + f.Kind + ")"
	}
	age := fmtAge(f.Age) + " ago"
	switch {
	case f.Status == "":
		return s + " ended " + age + ", final status unknown."
	case f.Status == backend.TaskCompleted:
		return s + " COMPLETED " + age + "."
	case !f.StepsKnown || f.StepIdx == 0:
		return fmt.Sprintf("%s %s %s, step detail unavailable.", s, f.Status, age)
	}
	s += fmt.Sprintf(" %s %s at step %d/%d (%s)", f.Status, age, f.StepIdx, f.StepCount, f.StepID)
	if f.ErrorMsg != "" {
		// An error_msg may end in its own full stop.
		return s + ": " + strings.TrimRight(f.ErrorMsg, ".") + "."
	}
	return s + "."
}
