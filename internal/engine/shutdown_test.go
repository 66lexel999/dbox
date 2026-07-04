package engine

import "testing"

// TestShouldAutoShutdown covers the gate that decides when "turn off when done"
// fires: only when armed, after a real completion, and with nothing left to do.
// It exercises the pure predicate so no actual OS shutdown is ever issued.
func TestShouldAutoShutdown(t *testing.T) {
	e := newSchedTestEngine(t)
	set := func(status Status, later bool) {
		e.tasks = map[string]*Task{"x": {ID: "x", Status: status, Later: later}}
	}

	// Not armed → never fires, even when idle and completed.
	e.shutdownArmed = false
	e.completionsSinceArm = 1
	set(StatusCompleted, false)
	if e.shouldAutoShutdownLocked() {
		t.Fatal("fired while disarmed")
	}

	// Armed but nothing completed since arming (e.g. armed while idle) → wait.
	e.shutdownArmed = true
	e.completionsSinceArm = 0
	if e.shouldAutoShutdownLocked() {
		t.Fatal("fired with no completion since arm")
	}

	// Armed + a completion happened + everything idle → fire.
	e.completionsSinceArm = 1
	set(StatusCompleted, false)
	if !e.shouldAutoShutdownLocked() {
		t.Fatal("did not fire when the batch finished")
	}

	// A still-running / queued / scheduled task blocks the shutdown.
	for _, st := range []Status{StatusDownloading, StatusQueued, StatusScheduled} {
		set(st, false)
		if e.shouldAutoShutdownLocked() {
			t.Fatalf("fired while a %s task was pending", st)
		}
	}

	// An interrupted (non-Later) paused task blocks; a held "Download Later"
	// paused item does NOT (it won't finish on its own).
	set(StatusPaused, false)
	if e.shouldAutoShutdownLocked() {
		t.Fatal("fired while a resumable paused task remained")
	}
	set(StatusPaused, true) // held Later item
	if !e.shouldAutoShutdownLocked() {
		t.Fatal("a held Download-Later item should not block shutdown")
	}

	// Arming resets the completion counter so a prior batch's completions don't
	// instantly re-fire.
	e.completionsSinceArm = 5
	e.SetShutdownWhenDone(true)
	if e.completionsSinceArm != 0 {
		t.Fatalf("arming did not reset the completion counter: got %d", e.completionsSinceArm)
	}
	if e.shouldAutoShutdownLocked() {
		t.Fatal("fired immediately after re-arming")
	}
}
