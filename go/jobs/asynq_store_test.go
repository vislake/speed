package jobs

import (
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/hibiken/asynq"

	"github.com/vislake/speed/go/pkgcore"
)

func TestQueueForPriority(t *testing.T) {
	tests := []struct {
		name string
		p    Priority
		want string
	}{
		{name: "low", p: PriorityLow, want: asynqQueueLow},
		{name: "normal", p: PriorityNormal, want: asynqQueueDefault},
		{name: "high", p: PriorityHigh, want: asynqQueueCritical},
		{name: "below low collapses to low", p: Priority(-5), want: asynqQueueLow},
		{name: "above high collapses to critical", p: Priority(100), want: asynqQueueCritical},
		{name: "between low and normal collapses to default", p: Priority(2), want: asynqQueueDefault},
		{name: "between normal and high collapses to default", p: Priority(7), want: asynqQueueDefault},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := queueForPriority(tt.p); got != tt.want {
				t.Errorf("queueForPriority(%d) = %q, want %q", tt.p, got, tt.want)
			}
		})
	}
}

func TestPriorityForQueue(t *testing.T) {
	tests := []struct {
		queue string
		want  Priority
	}{
		{queue: asynqQueueCritical, want: PriorityHigh},
		{queue: asynqQueueDefault, want: PriorityNormal},
		{queue: asynqQueueLow, want: PriorityLow},
		{queue: "some-unknown-queue", want: PriorityNormal},
	}
	for _, tt := range tests {
		t.Run(tt.queue, func(t *testing.T) {
			if got := priorityForQueue(tt.queue); got != tt.want {
				t.Errorf("priorityForQueue(%q) = %v, want %v", tt.queue, got, tt.want)
			}
		})
	}
}

func TestBuildTaskHeaders_And_HeaderCreatedAtTime_RoundTrip(t *testing.T) {
	now := time.Date(2026, 3, 4, 5, 6, 7, 890, time.UTC)
	task := Task{Type: "t", TenantID: pkgcore.TenantID("tenant-a"), IdempotencyKey: "op-1"}

	headers := buildTaskHeaders(task, now)

	if got := headers[headerTenantID]; got != "tenant-a" {
		t.Errorf("headers[tenant_id] = %q, want %q", got, "tenant-a")
	}
	if got := headers[headerIdempotencyKey]; got != "op-1" {
		t.Errorf("headers[idempotency_key] = %q, want %q", got, "op-1")
	}
	if got := headerCreatedAtTime(headers); !got.Equal(now) {
		t.Errorf("headerCreatedAtTime() = %v, want %v", got, now)
	}
}

func TestHeaderCreatedAtTime_MissingOrMalformed_ReturnsZeroTime(t *testing.T) {
	tests := map[string]map[string]string{
		"nil headers":     nil,
		"missing key":     {"other": "x"},
		"malformed value": {headerCreatedAt: "not-a-number"},
	}
	for name, headers := range tests {
		t.Run(name, func(t *testing.T) {
			if got := headerCreatedAtTime(headers); !got.IsZero() {
				t.Errorf("headerCreatedAtTime(%v) = %v, want zero time", headers, got)
			}
		})
	}
}

func TestEncodeDecodeResultEnvelope_RoundTrip(t *testing.T) {
	want := asynqResultEnvelope{StartedAt: 12345, ProgressPct: 42, ProgressMsg: "halfway", Data: []byte("done")}

	data, err := encodeResultEnvelope(want)
	if err != nil {
		t.Fatalf("encodeResultEnvelope() error = %v", err)
	}
	got := decodeResultEnvelope(data)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("decodeResultEnvelope(encodeResultEnvelope(want)) = %+v, want %+v", got, want)
	}
}

func TestDecodeResultEnvelope_EmptyOrMalformed_ReturnsZeroValue(t *testing.T) {
	tests := map[string][]byte{
		"nil":       nil,
		"empty":     {},
		"malformed": []byte("not json"),
	}
	for name, data := range tests {
		t.Run(name, func(t *testing.T) {
			if got := decodeResultEnvelope(data); !reflect.DeepEqual(got, asynqResultEnvelope{}) {
				t.Errorf("decodeResultEnvelope(%q) = %+v, want zero value", data, got)
			}
		})
	}
}

// TestAttemptsFromTaskInfo pins attemptsFromTaskInfo's per-state formula
// against the exact boundary asynq's own internal/rdb Retry/Archive Lua
// scripts implement (traced in attemptsFromTaskInfo's own doc comment): a
// future asynq upgrade that changed this contract would need this test
// updated deliberately, not silently drift past it.
func TestAttemptsFromTaskInfo(t *testing.T) {
	tests := []struct {
		name    string
		state   asynq.TaskState
		retried int
		want    int
	}{
		{name: "pending, never dequeued", state: asynq.TaskStatePending, retried: 0, want: 0},
		{name: "scheduled, never dequeued", state: asynq.TaskStateScheduled, retried: 0, want: 0},
		{name: "aggregating, never dequeued", state: asynq.TaskStateAggregating, retried: 0, want: 0},
		{name: "active, first attempt", state: asynq.TaskStateActive, retried: 0, want: 1},
		{name: "active, third attempt (two prior failures)", state: asynq.TaskStateActive, retried: 2, want: 3},
		{name: "retry, one failure banked", state: asynq.TaskStateRetry, retried: 1, want: 1},
		{name: "retry, three failures banked", state: asynq.TaskStateRetry, retried: 3, want: 3},
		{name: "completed, succeeded on first attempt", state: asynq.TaskStateCompleted, retried: 0, want: 1},
		{name: "completed, succeeded after two prior failures", state: asynq.TaskStateCompleted, retried: 2, want: 3},
		{name: "archived, MaxRetry=3 exhausted (total attempts = MaxRetry+1)", state: asynq.TaskStateArchived, retried: 3, want: 4},
		{name: "archived, MaxRetry=0 (single attempt dead-letters immediately)", state: asynq.TaskStateArchived, retried: 0, want: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info := &asynq.TaskInfo{State: tt.state, Retried: tt.retried}
			if got := attemptsFromTaskInfo(info); got != tt.want {
				t.Errorf("attemptsFromTaskInfo(state=%v, retried=%d) = %d, want %d", tt.state, tt.retried, got, tt.want)
			}
		})
	}
}

func TestStatusFromTaskState(t *testing.T) {
	tests := []struct {
		name      string
		state     asynq.TaskState
		cancelled bool
		want      Status
	}{
		{name: "pending", state: asynq.TaskStatePending, want: StatusPending},
		{name: "scheduled", state: asynq.TaskStateScheduled, want: StatusPending},
		{name: "aggregating", state: asynq.TaskStateAggregating, want: StatusPending},
		{name: "active", state: asynq.TaskStateActive, want: StatusRunning},
		{name: "retry", state: asynq.TaskStateRetry, want: StatusRetrying},
		{name: "completed", state: asynq.TaskStateCompleted, want: StatusSucceeded},
		{name: "archived", state: asynq.TaskStateArchived, want: StatusDeadLetter},
		{name: "cancelled wins over pending", state: asynq.TaskStatePending, cancelled: true, want: StatusCancelled},
		{name: "cancelled wins over active", state: asynq.TaskStateActive, cancelled: true, want: StatusCancelled},
		{name: "cancelled wins over retry", state: asynq.TaskStateRetry, cancelled: true, want: StatusCancelled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := statusFromTaskState(tt.state, tt.cancelled); got != tt.want {
				t.Errorf("statusFromTaskState(%v, %v) = %v, want %v", tt.state, tt.cancelled, got, tt.want)
			}
		})
	}
}

func TestAsynqJobFromTaskInfo_Succeeded(t *testing.T) {
	completedAt := time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)
	createdAt := completedAt.Add(-time.Minute)
	env, err := encodeResultEnvelope(asynqResultEnvelope{ProgressPct: 100, ProgressMsg: "done", Data: []byte("result-bytes")})
	if err != nil {
		t.Fatalf("encodeResultEnvelope() error = %v", err)
	}
	info := &asynq.TaskInfo{
		ID:      "job-1",
		Queue:   asynqQueueCritical,
		Type:    "greet",
		Payload: []byte("payload"),
		Headers: map[string]string{
			headerTenantID:       "tenant-a",
			headerIdempotencyKey: "op-1",
			headerCreatedAt:      strconv.FormatInt(createdAt.UnixNano(), 10),
		},
		State:       asynq.TaskStateCompleted,
		MaxRetry:    3,
		Retried:     1,
		CompletedAt: completedAt,
		Result:      env,
	}

	job := asynqJobFromTaskInfo(info, nil)

	if job.Status != StatusSucceeded {
		t.Errorf("Status = %v, want %v", job.Status, StatusSucceeded)
	}
	if job.TenantID != pkgcore.TenantID("tenant-a") {
		t.Errorf("TenantID = %v, want tenant-a", job.TenantID)
	}
	if job.IdempotencyKey != "op-1" {
		t.Errorf("IdempotencyKey = %q, want op-1", job.IdempotencyKey)
	}
	if job.Priority != PriorityHigh {
		t.Errorf("Priority = %v, want %v (queue=%q)", job.Priority, PriorityHigh, info.Queue)
	}
	if job.Attempts != 2 {
		t.Errorf("Attempts = %d, want 2", job.Attempts)
	}
	if job.Result == nil || string(job.Result.Data) != "result-bytes" {
		t.Errorf("Result = %+v, want Data = %q", job.Result, "result-bytes")
	}
	if job.ProgressPct != 100 || job.ProgressMsg != "done" {
		t.Errorf("progress = (%d, %q), want (100, %q)", job.ProgressPct, job.ProgressMsg, "done")
	}
	if job.Error != "" {
		t.Errorf("Error = %q, want empty on success", job.Error)
	}
	if job.CompletedAt == nil || !job.CompletedAt.Equal(completedAt) {
		t.Errorf("CompletedAt = %v, want %v", job.CompletedAt, completedAt)
	}
	if !job.CreatedAt.Equal(createdAt) {
		t.Errorf("CreatedAt = %v, want %v", job.CreatedAt, createdAt)
	}
}

func TestAsynqJobFromTaskInfo_DeadLetter(t *testing.T) {
	lastFailedAt := time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)
	info := &asynq.TaskInfo{
		ID:           "job-2",
		Queue:        asynqQueueDefault,
		Type:         "always-fails",
		Headers:      map[string]string{headerTenantID: "tenant-a"},
		State:        asynq.TaskStateArchived,
		MaxRetry:     1,
		Retried:      1,
		LastErr:      "permanent failure",
		LastFailedAt: lastFailedAt,
	}

	job := asynqJobFromTaskInfo(info, nil)

	if job.Status != StatusDeadLetter {
		t.Errorf("Status = %v, want %v", job.Status, StatusDeadLetter)
	}
	if job.Attempts != 2 {
		t.Errorf("Attempts = %d, want 2 (1 initial + 1 retry, MaxRetry=1)", job.Attempts)
	}
	if job.Error != "permanent failure" {
		t.Errorf("Error = %q, want %q", job.Error, "permanent failure")
	}
	if job.Result != nil {
		t.Errorf("Result = %+v, want nil for a dead-lettered job", job.Result)
	}
	if job.CompletedAt == nil || !job.CompletedAt.Equal(lastFailedAt) {
		t.Errorf("CompletedAt = %v, want %v", job.CompletedAt, lastFailedAt)
	}
}

func TestAsynqJobFromTaskInfo_CancelledOverridesUnderlyingState(t *testing.T) {
	cancelledAt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	info := &asynq.TaskInfo{
		ID:      "job-3",
		Queue:   asynqQueueDefault,
		Type:    "flood",
		Headers: map[string]string{headerTenantID: "tenant-a"},
		State:   asynq.TaskStateRetry, // still "alive" from asynq's own perspective.
		LastErr: "a prior attempt's failure",
	}

	job := asynqJobFromTaskInfo(info, &cancelledAt)

	if job.Status != StatusCancelled {
		t.Errorf("Status = %v, want %v even though the underlying asynq state is %v", job.Status, StatusCancelled, info.State)
	}
	if job.CompletedAt == nil || !job.CompletedAt.Equal(cancelledAt) {
		t.Errorf("CompletedAt = %v, want %v", job.CompletedAt, cancelledAt)
	}
	if job.Error != "a prior attempt's failure" {
		t.Errorf("Error = %q, want the prior attempt's message preserved (Job.Error's own doc comment: Cancel does not clear it)", job.Error)
	}
}
