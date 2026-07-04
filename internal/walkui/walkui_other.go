//go:build !windows

// Stub so the package builds on non-Windows. lxn/walk is Windows-only; MyIDM's
// native UI is therefore Windows-only and these calls are no-ops elsewhere
// (the headless/server path is used instead).

package walkui

import (
	"log/slog"
	"net/url"
	"time"

	"myidm/internal/engine"
)

type Engine interface {
	List() []engine.TaskView
	Categories() map[string]string
	CategoryDir(name string) string
	Add(rawURL, fileName string, segments int, dir string, scheduledAt time.Time, later bool) (engine.TaskView, error)
	Pause(id string) error
	Resume(id string) error
	Delete(id string, removeFile bool) error
	RenameFile(id, newName string) (engine.TaskView, error)
	FilePath(id string) (string, error)
}

type PopupEngine interface {
	Add(rawURL, fileName string, segments int, dir string, scheduledAt time.Time, later bool) (engine.TaskView, error)
	AddVideo(rawURL, title, selector, ext, dir string, audio bool, scheduledAt time.Time, later bool) (engine.TaskView, error)
	Categories() map[string]string
	CategoryDir(name string) string
	Get(id string) (engine.TaskView, error)
	FilePath(id string) (string, error)
	Pause(id string) error
	Resume(id string) error
	Delete(id string, removeFile bool) error
	SuppressCompletionPopup(d time.Duration)
}

type App struct{}

func New(eng Engine, log *slog.Logger, onPrompt func(string), onDetail func(string)) *App {
	return &App{}
}

func RunBlocking(a *App, onClose func()) {
	if onClose != nil {
		onClose()
	}
}

func OpenDialog(eng PopupEngine, log *slog.Logger, q url.Values) {}
func OpenDetail(eng PopupEngine, log *slog.Logger, id string)    {}
func OpenDone(eng PopupEngine, log *slog.Logger, id string)      {}
