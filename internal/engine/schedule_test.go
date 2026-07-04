package engine

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"myidm/internal/config"
	"myidm/internal/store"
)

// newSchedTestEngine builds a minimal engine wired with a real store + discard
// logger, enough to exercise the scheduling lifecycle (no workers run).
func newSchedTestEngine(t *testing.T) *Engine {
	t.Helper()
	dataDir := t.TempDir()
	st, err := store.New(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	cfg := &config.Config{DataDir: dataDir, DownloadDir: base, Categories: config.DefaultCategories(base), Segments: 8, MaxConcurrent: 3}
	return &Engine{
		cfg:       cfg,
		store:     st,
		log:       slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError})),
		tasks:     map[string]*Task{},
		kick:      make(chan struct{}, 1),
		overrides: map[string]string{},
	}
}

// TestAddScheduledHoldsTask verifies a future schedule keeps a new download out
// of the run queue until promoted, and that promotion fires once it's due.
func TestAddScheduledHoldsTask(t *testing.T) {
	e := newSchedTestEngine(t)

	future := time.Now().Add(time.Hour)
	v, err := e.Add("https://example.com/a.bin", "a.bin", 4, "", future, false)
	if err != nil {
		t.Fatal(err)
	}
	if v.Status != StatusScheduled {
		t.Fatalf("status = %q, want scheduled", v.Status)
	}
	if v.ScheduledAt == nil || !v.ScheduledAt.Equal(future) {
		t.Fatalf("ScheduledAt = %v, want %v", v.ScheduledAt, future)
	}
	if len(e.queue) != 0 {
		t.Fatalf("scheduled task should NOT be queued yet; queue=%v", e.queue)
	}

	// Not due yet → no promotion.
	if e.promoteDueLocked(time.Now()) {
		t.Fatal("promoted a task that isn't due")
	}
	// Due → promoted into the queue, schedule cleared, status queued.
	if !e.promoteDueLocked(future.Add(time.Second)) {
		t.Fatal("did not promote a due task")
	}
	got, _ := e.Get(v.ID)
	if got.Status != StatusQueued {
		t.Fatalf("after promotion status = %q, want queued", got.Status)
	}
	if got.ScheduledAt != nil {
		t.Fatalf("ScheduledAt should be cleared after promotion, got %v", got.ScheduledAt)
	}
	if len(e.queue) != 1 || e.queue[0] != v.ID {
		t.Fatalf("queue = %v, want [%s]", e.queue, v.ID)
	}
}

// TestAddPastScheduleQueuesNow verifies a zero/past time falls through to the
// normal "queue now" path.
func TestAddPastScheduleQueuesNow(t *testing.T) {
	e := newSchedTestEngine(t)
	for _, at := range []time.Time{{}, time.Now().Add(-time.Hour)} {
		v, err := e.Add("https://example.com/x", "", 0, "", at, false)
		if err != nil {
			t.Fatal(err)
		}
		if v.Status != StatusQueued {
			t.Fatalf("status = %q for at=%v, want queued", v.Status, at)
		}
	}
	if len(e.queue) != 2 {
		t.Fatalf("queue len = %d, want 2", len(e.queue))
	}
}

// TestScheduleUnscheduleStartNow walks an existing task through the scheduler
// management actions used by the UI.
func TestScheduleUnscheduleStartNow(t *testing.T) {
	e := newSchedTestEngine(t)
	v, err := e.Add("https://example.com/b.bin", "b.bin", 0, "", time.Time{}, false)
	if err != nil {
		t.Fatal(err)
	}
	id := v.ID
	// Pause so it's in a schedulable, non-queued state (mirrors the UI flow).
	if err := e.Pause(id); err != nil {
		t.Fatal(err)
	}

	at := time.Now().Add(2 * time.Hour)
	if err := e.Schedule(id, at); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.Get(id); got.Status != StatusScheduled || got.ScheduledAt == nil {
		t.Fatalf("after Schedule: status=%q scheduledAt=%v", got.Status, got.ScheduledAt)
	}
	if contains(e.queue, id) {
		t.Fatal("a scheduled task must not sit in the run queue")
	}

	// Unschedule parks it as paused.
	if err := e.Unschedule(id); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.Get(id); got.Status != StatusPaused || got.ScheduledAt != nil {
		t.Fatalf("after Unschedule: status=%q scheduledAt=%v", got.Status, got.ScheduledAt)
	}

	// Re-schedule, then "Start now" (Schedule with a past time) queues it.
	if err := e.Schedule(id, at); err != nil {
		t.Fatal(err)
	}
	if err := e.Schedule(id, time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.Get(id); got.Status != StatusQueued || got.ScheduledAt != nil {
		t.Fatalf("after start-now: status=%q scheduledAt=%v", got.Status, got.ScheduledAt)
	}
	if !contains(e.queue, id) {
		t.Fatal("started task should be in the run queue")
	}

	// A completed task can't be scheduled.
	e.tasks[id].Status = StatusCompleted
	if err := e.Schedule(id, at); err == nil {
		t.Fatal("scheduling a completed task should fail")
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
