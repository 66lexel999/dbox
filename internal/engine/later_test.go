package engine

import (
	"testing"
	"time"
)

// TestAddLaterHoldsTask verifies a Download Later add is held (paused, flagged
// later) and never enters the run queue until started.
func TestAddLaterHoldsTask(t *testing.T) {
	e := newSchedTestEngine(t)

	v, err := e.Add("https://example.com/a.bin", "a.bin", 4, "", time.Time{}, true)
	if err != nil {
		t.Fatal(err)
	}
	if v.Status != StatusPaused || !v.Later {
		t.Fatalf("status=%q later=%v, want paused+later", v.Status, v.Later)
	}
	if len(e.queue) != 0 {
		t.Fatalf("a Download Later task must not be queued; queue=%v", e.queue)
	}

	// Skip toggling is reflected in the view.
	if err := e.SetSkip(v.ID, true); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.Get(v.ID); !got.Skip {
		t.Fatal("SetSkip(true) did not set the skip flag")
	}
	if err := e.SetSkip(v.ID, false); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.Get(v.ID); got.Skip {
		t.Fatal("SetSkip(false) did not clear the skip flag")
	}

	// "Start" from the queue = Resume → enters the run queue, still flagged later.
	if err := e.Resume(v.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := e.Get(v.ID)
	if got.Status != StatusQueued {
		t.Fatalf("after start status=%q, want queued", got.Status)
	}
	if !got.Later {
		t.Fatal("the later flag should persist while the queued item downloads")
	}
	if !contains(e.queue, v.ID) {
		t.Fatal("started later-item should be in the run queue")
	}
}

// TestSetMaxConcurrent verifies the simultaneous-download limit is clamped,
// persisted, and reloaded.
func TestSetMaxConcurrent(t *testing.T) {
	e := newSchedTestEngine(t)

	if err := e.SetMaxConcurrent(5); err != nil {
		t.Fatal(err)
	}
	if got := e.maxConcurrent(); got != 5 {
		t.Fatalf("after set, maxConcurrent = %d, want 5", got)
	}
	// Clamp out-of-range.
	e.SetMaxConcurrent(0)
	if got := e.maxConcurrent(); got != 1 {
		t.Fatalf("clamp low: got %d, want 1", got)
	}
	e.SetMaxConcurrent(999)
	if got := e.maxConcurrent(); got != 16 {
		t.Fatalf("clamp high: got %d, want 16", got)
	}

	// Persists across a reload over the same data dir.
	e.SetMaxConcurrent(4)
	e2 := newSchedTestEngine(t)
	e2.cfg.DataDir = e.cfg.DataDir
	e2.loadSettings()
	if got := e2.maxConcurrent(); got != 4 {
		t.Fatalf("reloaded maxConcurrent = %d, want 4", got)
	}
}
