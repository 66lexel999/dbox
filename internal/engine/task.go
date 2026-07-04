package engine

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"time"
)

type Status string

const (
	StatusQueued      Status = "queued"
	StatusDownloading Status = "downloading"
	StatusPaused      Status = "paused"
	StatusCompleted   Status = "completed"
	StatusFailed      Status = "failed"
	StatusScheduled   Status = "scheduled" // waiting for ScheduledAt to arrive, then auto-queues
)

// intent records why a running task's context was cancelled so runTask can
// pick the right terminal state.
type intent int

const (
	intentNone intent = iota
	intentPause
	intentDelete
	intentShutdown
)

// Segment is one byte range of the target file. Done is updated lock-free by
// its download worker and read concurrently by snapshots/persistence.
type Segment struct {
	Start int64
	End   int64 // inclusive; -1 when total size is unknown
	done  atomic.Int64
}

func (s *Segment) Done() int64     { return s.done.Load() }
func (s *Segment) SetDone(v int64) { s.done.Store(v) }

// Length returns the segment size in bytes, or -1 if unbounded.
func (s *Segment) Length() int64 {
	if s.End < 0 {
		return -1
	}
	return s.End - s.Start + 1
}

func (s *Segment) Remaining() int64 {
	if s.End < 0 {
		return -1
	}
	r := s.Length() - s.Done()
	if r < 0 {
		r = 0
	}
	return r
}

type segmentJSON struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"`
	Done  int64 `json:"done"`
}

func (s *Segment) MarshalJSON() ([]byte, error) {
	return json.Marshal(segmentJSON{Start: s.Start, End: s.End, Done: s.done.Load()})
}

func (s *Segment) UnmarshalJSON(b []byte) error {
	var j segmentJSON
	if err := json.Unmarshal(b, &j); err != nil {
		return err
	}
	s.Start, s.End = j.Start, j.End
	s.done.Store(j.Done)
	return nil
}

// Task is the persisted + runtime state of one download.
// Exported fields are persisted to tasks.json; unexported fields are runtime only.
type Task struct {
	ID           string     `json:"id"`
	URL          string     `json:"url"`
	FileName     string     `json:"fileName"` // final on-disk name (resolved at probe)
	Dir          string     `json:"dir"`
	Size         int64      `json:"size"` // -1 unknown
	Ranged       bool       `json:"ranged"`
	Probed       bool       `json:"probed"`
	ETag         string     `json:"etag,omitempty"`
	LastModified string     `json:"lastModified,omitempty"`
	ContentType  string     `json:"contentType,omitempty"`
	Status       Status     `json:"status"`
	Error        string     `json:"error,omitempty"`
	Segments     []*Segment `json:"segments"`
	WantSegments int        `json:"wantSegments"`
	CreatedAt    time.Time  `json:"createdAt"`
	CompletedAt  *time.Time `json:"completedAt,omitempty"`
	ScheduledAt  *time.Time `json:"scheduledAt,omitempty"` // set while StatusScheduled; auto-queues when reached
	Later        bool       `json:"later,omitempty"`       // held in the "Download Later" queue (start manually)
	Skip         bool       `json:"skip,omitempty"`        // marked to be skipped by "Start All"
	Description  string     `json:"description,omitempty"` // user note from the New Download dialog
	FinalPath    string     `json:"finalPath,omitempty"`   // set once completed

	// yt-dlp tasks (streaming-site video/audio via the external binary)
	Kind     string `json:"kind,omitempty"`     // "" = http segmented; "ytdlp"
	Selector string `json:"selector,omitempty"` // yt-dlp -f selector
	Title    string `json:"title,omitempty"`
	Audio    bool   `json:"audio,omitempty"`

	// runtime
	cancel     context.CancelFunc
	intent     intent
	deleteFile bool
	prevBytes  int64   // sampler state
	speed      float64 // bytes/sec, EMA
	// speed stats for the current run (reset when the download (re)starts).
	// speedMax is the all-time peak; avg/min are computed from `recent`, a
	// rolling window of the latest EMA samples, so they reflect current
	// steady-state speed rather than being dragged down forever by the
	// multi-connection ramp-up (and brief video→audio progress resets) at the
	// start of the transfer.
	speedMax float64   // peak EMA bytes/sec this run (all-time)
	recent   []float64 // sliding window of recent EMA samples (data-flowing ticks)
}

// downloaded sums segment progress. Safe to call concurrently.
func (t *Task) downloaded() int64 {
	var n int64
	for _, s := range t.Segments {
		n += s.Done()
	}
	return n
}

// SegmentView is the API-facing snapshot of a Segment.
type SegmentView struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"`
	Done  int64 `json:"done"`
}

// TaskView is the JSON snapshot served to the UI.
type TaskView struct {
	ID          string        `json:"id"`
	URL         string        `json:"url"`
	FileName    string        `json:"fileName"`
	Dir         string        `json:"dir"`
	Size        int64         `json:"size"`
	Ranged      bool          `json:"ranged"`
	Status      Status        `json:"status"`
	Kind        string        `json:"kind,omitempty"`
	Error       string        `json:"error,omitempty"`
	Downloaded  int64         `json:"downloaded"`
	Speed       float64       `json:"speed"`    // bytes/sec (current, EMA)
	SpeedAvg    float64       `json:"speedAvg"` // bytes/sec averaged over active time
	SpeedMax    float64       `json:"speedMax"` // peak bytes/sec this run
	SpeedMin    float64       `json:"speedMin"` // lowest non-zero bytes/sec this run
	ETA         float64       `json:"eta"`      // seconds, -1 unknown
	Progress    float64       `json:"progress"` // 0..1, -1 unknown
	Segments    []SegmentView `json:"segments"`
	CreatedAt   time.Time     `json:"createdAt"`
	CompletedAt *time.Time    `json:"completedAt,omitempty"`
	ScheduledAt *time.Time    `json:"scheduledAt,omitempty"`
	Later       bool          `json:"later,omitempty"`
	Skip        bool          `json:"skip,omitempty"`
	Description string        `json:"description,omitempty"`
	FilePath    string        `json:"filePath,omitempty"`
	FileExists  bool          `json:"fileExists"`
}
