package backend

import (
	"context"
	"net/url"
	"time"
)

// Task statuses (upper case on the wire). GET only ever returns these five;
// PENDING / PAUSING / CANCELING appear only in the POST / DELETE acks.
const (
	TaskInProgress = "IN_PROGRESS"
	TaskPaused     = "PAUSED"
	TaskCompleted  = "COMPLETED"
	TaskFailed     = "FAILED"
	TaskCanceled   = "CANCELED"
)

// SourceSchedule is the source of a task a schedule fired; a manual one is "DIRECT".
const SourceSchedule = "SCHEDULE"

// ActiveTask is one entry of GET /api/v1/active_tasks: identity and provenance, no steps.
// kind / name / map_name / schedule_id may be null, which decodes to "".
type ActiveTask struct {
	ID string `json:"id"`
	// A held task is still listed as IN_PROGRESS; PAUSED is visible only through GetTaskState.
	Status     string    `json:"status"`
	StartedAt  time.Time `json:"started_at"`
	Source     string    `json:"source"`
	ScheduleID string    `json:"schedule_id"`
	Kind       string    `json:"kind"` // lower case: goal / standup / liedown / task / schedule
	Name       string    `json:"name"`
	MapName    string    `json:"map_name"`
}

// ActiveTasks is the response of GET /api/v1/active_tasks.
type ActiveTasks struct {
	Tasks []ActiveTask `json:"tasks"`
	// AsOf is when the backend ran the query. Ages must be computed from it,
	// not from the local clock.
	AsOf time.Time `json:"as_of"`
}

// StepState is the state of one step of a task. A step carries no type.
type StepState struct {
	ID       string `json:"id"`
	Status   string `json:"status"`
	ErrorMsg string `json:"error_msg"`
}

// TaskState is the response of GET /api/v1/tasks/{id}.
// Steps is empty when the workflow query failed.
type TaskState struct {
	ID     string      `json:"id"`
	Status string      `json:"status"`
	Steps  []StepState `json:"steps"`
}

// GetActiveTasks lists the tasks running now.
// An empty list is a valid answer (never 404); the backend caches it for 1.5 s;
// with Temporal down it answers 502 rather than a stale cache.
func (c *Client) GetActiveTasks(ctx context.Context) (ActiveTasks, error) {
	var out ActiveTasks
	err := c.getJSON(ctx, "/api/v1/active_tasks", &out)
	return out, err
}

// GetTaskState reads one task and the live state of each of its steps.
// A finished task is still answered within Temporal's retention; an id that does
// not exist or belongs to another robot is ErrNotFound.
func (c *Client) GetTaskState(ctx context.Context, id string) (TaskState, error) {
	var out TaskState
	err := c.getJSON(ctx, "/api/v1/tasks/"+url.PathEscape(id), &out)
	return out, err
}
