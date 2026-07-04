package engine

import (
	"path/filepath"
	"testing"

	"myidm/internal/config"
)

// TestFolderSettings exercises the Options folder feature end to end at the
// engine layer: pin a category ("remember this path"), change the base folder
// (pinned stays, others follow), persistence across a reload, and reset.
func TestFolderSettings(t *testing.T) {
	dataDir := t.TempDir()
	base := filepath.Join(t.TempDir(), "dl")
	def := filepath.Join(t.TempDir(), "default")
	cfg := &config.Config{DataDir: dataDir, DownloadDir: base, Categories: config.DefaultCategories(base)}
	e := &Engine{cfg: cfg, overrides: map[string]string{}, defaultDir: def}

	// Pin Video to a custom folder ("remember this path for the category").
	custom := filepath.Join(t.TempDir(), "movies")
	if err := e.SetCategoryDir("Video", custom); err != nil {
		t.Fatal(err)
	}
	if got := e.CategoryDir("Video"); got != custom {
		t.Fatalf("Video dir = %q, want pinned %q", got, custom)
	}

	// Change the base: default-derived categories move under it, the pinned one stays.
	newBase := filepath.Join(t.TempDir(), "newbase")
	if err := e.SetDownloadDir(newBase); err != nil {
		t.Fatal(err)
	}
	if got, want := e.CategoryDir("Music"), filepath.Join(newBase, "Music"); got != want {
		t.Fatalf("Music should follow base: got %q want %q", got, want)
	}
	if got := e.CategoryDir("Video"); got != custom {
		t.Fatalf("Video should keep its pinned folder across a base change: got %q", got)
	}
	if got := e.downloadDir(); got != newBase {
		t.Fatalf("base = %q, want %q", got, newBase)
	}

	// Settings persist: a fresh engine over the same data dir reloads them.
	e2 := &Engine{cfg: &config.Config{DataDir: dataDir, DownloadDir: "overridden-on-load"}, overrides: map[string]string{}, defaultDir: def}
	e2.loadSettings()
	if got := e2.downloadDir(); got != newBase {
		t.Fatalf("reloaded base = %q, want %q", got, newBase)
	}
	if got := e2.CategoryDir("Video"); got != custom {
		t.Fatalf("reloaded Video = %q, want %q", got, custom)
	}

	// Reset: base back to the app default, pinned folders cleared.
	if err := e.ResetPaths(); err != nil {
		t.Fatal(err)
	}
	if got := e.downloadDir(); got != def {
		t.Fatalf("after reset base = %q, want default %q", got, def)
	}
	if got, want := e.CategoryDir("Video"), filepath.Join(def, "Video"); got != want {
		t.Fatalf("after reset Video = %q, want %q", got, want)
	}
}
