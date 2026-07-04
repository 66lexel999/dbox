// Package server exposes the engine over a local REST API + SSE stream and
// serves the embedded single-file web UI (stand-in for the reference repo's
// gRPC-gateway + React SPA).
package server

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"myidm/internal/engine"
	"myidm/internal/procutil"
	"myidm/internal/updater"
	"myidm/internal/ytdlp"
)

//go:embed web/index.html
var indexHTML []byte

//go:embed web/add.html
var addHTML []byte

//go:embed web/done.html
var doneHTML []byte

//go:embed web/detail.html
var detailHTML []byte

//go:embed web/icons/appicon.png
var appiconPNG []byte

//go:embed web/icons/picture.png
var picturePNG []byte

//go:embed web/icons/video.png
var videoPNG []byte

//go:embed web/icons/compress.png
var compressPNG []byte

// extCategory maps a lowercase file extension to an IDM-style category name,
// so the add dialog can preselect the right category. General is the fallback.
var extCategory = map[string]string{
	"zip": "Compressed", "rar": "Compressed", "7z": "Compressed", "tar": "Compressed",
	"gz": "Compressed", "bz2": "Compressed", "xz": "Compressed", "zst": "Compressed",
	"iso": "Compressed", "cab": "Compressed",
	"pdf": "Documents", "doc": "Documents", "docx": "Documents", "xls": "Documents",
	"xlsx": "Documents", "ppt": "Documents", "pptx": "Documents", "txt": "Documents",
	"odt": "Documents", "csv": "Documents", "epub": "Documents", "rtf": "Documents",
	"mp3": "Music", "wav": "Music", "flac": "Music", "aac": "Music", "ogg": "Music",
	"m4a": "Music", "wma": "Music", "opus": "Music",
	"mp4": "Video", "mkv": "Video", "avi": "Video", "mov": "Video", "webm": "Video",
	"flv": "Video", "wmv": "Video", "m4v": "Video", "mpg": "Video", "mpeg": "Video",
	"ts": "Video", "3gp": "Video",
	"exe": "Programs", "msi": "Programs", "apk": "Programs", "dmg": "Programs",
	"deb": "Programs", "rpm": "Programs", "appimage": "Programs", "msix": "Programs",
	"bat": "Programs", "jar": "Programs",
	"jpg": "Images", "jpeg": "Images", "png": "Images", "gif": "Images", "webp": "Images",
	"bmp": "Images", "svg": "Images", "tiff": "Images", "ico": "Images", "heic": "Images",
	"psd": "Images",
}

// categoryOrder is the display order of categories in the add dialog.
var categoryOrder = []string{"General", "Compressed", "Documents", "Music", "Video", "Programs", "Images"}

type Server struct {
	eng    *engine.Engine
	log    *slog.Logger
	probes *probeCache
	dialog func(q url.Values) error             // opens the native New Download window; nil = headless
	detail func(id string) error                // opens the native per-connection progress window; nil = headless
	done   func(id string) error                // opens the native completion window on demand; nil = headless
	pick   func(initial string) (string, error) // native "choose folder" dialog (Options); nil = none
	quit   func()                               // request a real app exit (close-to-tray mode); nil = N/A

	fileIcon    func(path string) ([]byte, bool, error) // shell icon as PNG + degraded flag; nil = none
	iconGeneric func(ext string) bool                   // ext (".zip") has only a generic/Explorer default handler
	iconMu      sync.Mutex
	iconCache   map[string]iconEntry

	webMode        bool       // single-window HTML UI (Wails): relay prompts to the page over SSE
	promptMu       sync.Mutex // guards pendingPrompts
	pendingPrompts []string   // encoded query strings awaiting the frontend (POST /api/prompt from the extension)
	activate       func()     // brings the single window to the foreground (Wails); nil = no-op

	closeMu      sync.Mutex       // guards popupCloseAt
	popupCloseAt map[string]int64 // task id -> client wall-clock ms of the latest popup-close request

	updCurrent  string     // running version ("dev" disables updates)
	updManifest string     // latest.json URL ("" disables updates)
	updExit     func()     // fully exit the process (for the post-update relaunch)
	updMu       sync.Mutex // guards the fields below
	updProg     updater.Progress
	updApplying bool
}

// iconEntry is one memoized shell-icon extraction; png == nil records a miss,
// degraded marks an extension-fallback result — both retry after a short TTL
// (see iconFor).
type iconEntry struct {
	png      []byte
	at       time.Time
	degraded bool
}

func New(eng *engine.Engine, log *slog.Logger) *Server {
	s := &Server{eng: eng, log: log, popupCloseAt: map[string]int64{}}
	s.probes = newProbeCache(func(ctx context.Context, url string) (*ytdlp.ProbeResult, error) {
		return ytdlp.Probe(ctx, url, eng.UserAgent())
	})
	return s
}

// SetWebMode makes the server relay /api/prompt requests (e.g. from the browser
// extension) to the single-window HTML UI over SSE instead of opening a native
// window. Used by the Wails front end.
func (s *Server) SetWebMode(v bool) { s.webMode = v }

// SetActivateFunc registers a callback that brings the single window to the
// foreground (called when a New Download prompt arrives so the user sees it).
func (s *Server) SetActivateFunc(f func()) { s.activate = f }

// queuePrompt stores an encoded New Download query for the frontend to poll, and
// raises the window so the in-page dialog is visible.
func (s *Server) queuePrompt(q string) {
	s.promptMu.Lock()
	s.pendingPrompts = append(s.pendingPrompts, q)
	s.promptMu.Unlock()
	if s.activate != nil {
		s.activate()
	}
}

// drainPrompts returns and clears the queued prompts.
func (s *Server) drainPrompts() []string {
	s.promptMu.Lock()
	defer s.promptMu.Unlock()
	if len(s.pendingPrompts) == 0 {
		return nil
	}
	out := s.pendingPrompts
	s.pendingPrompts = nil
	return out
}

// SetDialogOpener wires the native "New Download" window. When set (GUI mode),
// POST /api/prompt opens it; when nil (headless), callers fall back to a popup.
func (s *Server) SetDialogOpener(f func(url.Values) error) { s.dialog = f }

// SetDetailOpener wires the native per-connection progress window (POST /api/detail).
func (s *Server) SetDetailOpener(f func(id string) error) { s.detail = f }

// SetDoneOpener wires the native completion window so it can be opened on demand
// (POST /api/done, used by the UI's "Preview" button). The engine's completion
// notifier pops the same window normally when a download finishes.
func (s *Server) SetDoneOpener(f func(id string) error) { s.done = f }

// SetFolderPicker wires the native "choose folder" dialog used by the Options
// panel (POST /api/pick-folder). nil = no native picker (UI falls back to typing).
func (s *Server) SetFolderPicker(f func(initial string) (string, error)) { s.pick = f }

// SetQuit wires a real application exit (File → Exit, when the window otherwise
// closes to the tray). nil = no-op.
func (s *Server) SetQuit(f func()) { s.quit = f }

// SetIconResolver wires native per-file icon extraction (GET /api/icon) and the
// "does this extension only have a generic default handler?" check. The icon
// func's degraded flag means "extension fallback used although the file exists"
// — cached briefly so the real icon can take over on a retry.
func (s *Server) SetIconResolver(icon func(string) ([]byte, bool, error), generic func(string) bool) {
	s.fileIcon, s.iconGeneric = icon, generic
}

// handleIcon serves the Windows shell icon for ?path= as a PNG. For an archive
// whose only handler is the generic Windows one (no WinRAR etc.), it 404s so the
// UI shows its own category icon instead.
func (s *Server) handleIcon(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" || s.fileIcon == nil {
		http.NotFound(w, r)
		return
	}
	ext := strings.ToLower(filepath.Ext(path))
	if extCategory[strings.TrimPrefix(ext, ".")] == "Compressed" && s.iconGeneric != nil && s.iconGeneric(ext) {
		http.NotFound(w, r)
		return
	}
	b := s.iconFor(path)
	if len(b) == 0 {
		http.NotFound(w, r)
		return
	}
	// Revalidate instead of long-lived caching: a degraded icon (extension
	// fallback while AV held the file) upgrades to the real one within ~45s, and
	// a fixed max-age would pin the stale image in WebView2 for an hour. The
	// ETag keeps the common case a 304 with no body.
	etag := fmt.Sprintf(`"%x-%x"`, len(b), fnv32(b))
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "no-cache")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Write(b)
}

// fnv32 is a tiny content hash for icon ETags.
func fnv32(b []byte) uint32 {
	h := uint32(2166136261)
	for _, c := range b {
		h = (h ^ uint32(c)) * 16777619
	}
	return h
}

// iconFor extracts (and memoizes) a file's icon. Clean successes cache forever.
// Failures AND degraded results (extension fallback while the file exists —
// e.g. AV still scanning a fresh .exe) cache only briefly, so the next attempt
// can produce the file's real icon instead of pinning a generic one all session.
func (s *Server) iconFor(path string) []byte {
	const retryTTL = 45 * time.Second
	s.iconMu.Lock()
	if e, ok := s.iconCache[path]; ok {
		if (e.png != nil && !e.degraded) || time.Since(e.at) < retryTTL {
			s.iconMu.Unlock()
			return e.png
		}
		delete(s.iconCache, path) // expired miss/degraded entry — try again
	}
	s.iconMu.Unlock()
	b, degraded, err := s.fileIcon(path)
	if err != nil {
		b = nil
	}
	s.iconMu.Lock()
	if s.iconCache == nil {
		s.iconCache = map[string]iconEntry{}
	}
	s.iconCache[path] = iconEntry{png: b, at: time.Now(), degraded: degraded}
	s.iconMu.Unlock()
	return b
}

// probeCache memoizes yt-dlp probe results so the browser overlay can prefetch
// a video's formats the moment its download button appears and then open the
// menu instantly. Concurrent callers for the same URL share one yt-dlp run
// (singleflight); the run uses a background context so a client that navigates
// away mid-prefetch still warms the cache for the next visitor.
type probeCache struct {
	mu      sync.Mutex
	entries map[string]*probeEntry
	okTTL   time.Duration
	errTTL  time.Duration
	probe   func(context.Context, string) (*ytdlp.ProbeResult, error)
}

type probeEntry struct {
	done chan struct{}
	res  *ytdlp.ProbeResult
	err  error
	at   time.Time // when res/err were stored (zero while in flight)
}

func newProbeCache(probe func(context.Context, string) (*ytdlp.ProbeResult, error)) *probeCache {
	return &probeCache{
		entries: make(map[string]*probeEntry),
		okTTL:   10 * time.Minute, // probe yields stable metadata; URLs re-resolve at download time
		errTTL:  15 * time.Second, // let failures retry soon without hammering yt-dlp
		probe:   probe,
	}
}

// get returns a cached or freshly-probed result for url, deduping concurrent
// callers. waitCtx bounds only the caller's wait, never the probe itself.
func (c *probeCache) get(waitCtx context.Context, url string) (*ytdlp.ProbeResult, error) {
	c.mu.Lock()
	if e := c.entries[url]; e != nil && e.fresh(c.okTTL, c.errTTL) {
		c.mu.Unlock()
		return e.wait(waitCtx)
	}
	if len(c.entries) > 64 { // opportunistic prune of expired entries
		for k, ent := range c.entries {
			if !ent.fresh(c.okTTL, c.errTTL) {
				delete(c.entries, k)
			}
		}
	}
	e := &probeEntry{done: make(chan struct{})}
	c.entries[url] = e
	c.mu.Unlock()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		res, err := c.probe(ctx, url)
		c.mu.Lock()
		e.res, e.err, e.at = res, err, time.Now()
		c.mu.Unlock()
		close(e.done)
	}()
	return e.wait(waitCtx)
}

// forget drops cached entries whose URL matches pred — used when fresh cookies
// arrive so a prior anonymous failure re-probes at once instead of serving the
// stale "login required" for the rest of its error-TTL.
func (c *probeCache) forget(pred func(url string) bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, e := range c.entries {
		select {
		case <-e.done: // only evict finished entries; leave in-flight singleflights alone
			if pred(k) {
				delete(c.entries, k)
			}
		default:
		}
	}
}

// fresh reports whether the entry is still in flight or within its TTL.
// Caller holds c.mu.
func (e *probeEntry) fresh(okTTL, errTTL time.Duration) bool {
	select {
	case <-e.done:
		ttl := okTTL
		if e.err != nil {
			ttl = errTTL
		}
		return time.Since(e.at) < ttl
	default:
		return true // still running — reuse it
	}
}

// wait blocks until the probe completes or the caller's context is done.
func (e *probeEntry) wait(ctx context.Context) (*ytdlp.ProbeResult, error) {
	select {
	case <-e.done:
		return e.res, e.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(indexHTML)
	})
	// Static icon assets (menu-bar logo, browser tab, the Images category icon).
	pngAsset := func(b []byte) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "image/png")
			// no-cache (revalidate) so a rebuilt icon shows immediately rather than
			// being served stale from WebView2's disk cache at the same URL.
			w.Header().Set("Cache-Control", "no-cache")
			w.Write(b)
		}
	}
	mux.HandleFunc("GET /icons/appicon.png", pngAsset(appiconPNG))
	mux.HandleFunc("GET /icons/picture.png", pngAsset(picturePNG))
	mux.HandleFunc("GET /icons/video.png", pngAsset(videoPNG))
	mux.HandleFunc("GET /icons/compress.png", pngAsset(compressPNG))
	mux.HandleFunc("GET /favicon.ico", pngAsset(appiconPNG))
	// Real per-file Windows shell icon (the app's own icon for an .exe, the
	// default associated app's for others) — rendered in the downloads table.
	mux.HandleFunc("GET /api/icon", s.handleIcon)
	// /add is the IDM-style "New Download" dialog the browser extension opens.
	mux.HandleFunc("GET /add", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(addHTML)
	})
	// /done and /detail back the native completion + per-connection windows.
	mux.HandleFunc("GET /done", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(doneHTML)
	})
	mux.HandleFunc("GET /detail", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(detailHTML)
	})
	mux.HandleFunc("POST /api/done-suppress", s.handleDoneSuppress)
	mux.HandleFunc("POST /api/detail", s.handleOpenDetail)
	mux.HandleFunc("POST /api/done", s.handleOpenDone)
	// Popup close coordination: the completion + status windows for one download
	// are siblings — closing either closes both (they poll GET, signal via POST).
	mux.HandleFunc("GET /api/popups/{id}/close", s.handlePopupCloseGet)
	mux.HandleFunc("POST /api/popups/{id}/close", s.handlePopupClose)
	mux.HandleFunc("GET /api/categories", s.handleCategories)
	// Folder settings (Options panel): change the base download folder, pin a
	// category to a remembered folder, reset all paths, and the native picker.
	mux.HandleFunc("GET /api/settings", s.handleGetSettings)
	mux.HandleFunc("POST /api/settings/dir", s.handleSetDir)
	mux.HandleFunc("POST /api/settings/category", s.handleSetCategory)
	mux.HandleFunc("POST /api/settings/reset", s.handleResetPaths)
	mux.HandleFunc("POST /api/settings/concurrent", s.handleSetConcurrent)
	mux.HandleFunc("POST /api/settings/shutdown", s.handleSetShutdown)
	mux.HandleFunc("POST /api/pick-folder", s.handlePickFolder)
	mux.HandleFunc("POST /api/quit", s.handleQuit)
	mux.HandleFunc("GET /api/update/check", s.handleUpdateCheck)
	mux.HandleFunc("POST /api/update/apply", s.handleUpdateApply)
	mux.HandleFunc("GET /api/update/status", s.handleUpdateStatus)
	mux.HandleFunc("GET /api/probe", s.handleProbe)
	mux.HandleFunc("POST /api/cookies", s.handleCookies)
	mux.HandleFunc("POST /api/video", s.handleVideo)
	mux.HandleFunc("GET /api/playlist", s.handlePlaylistInfo)
	mux.HandleFunc("POST /api/playlist", s.handlePlaylistDownload)
	mux.HandleFunc("GET /api/tasks", s.handleList)
	mux.HandleFunc("POST /api/tasks", s.handleCreate)
	mux.HandleFunc("GET /api/tasks/{id}", s.handleGet)
	mux.HandleFunc("POST /api/tasks/{id}/pause", s.handlePause)
	mux.HandleFunc("POST /api/tasks/{id}/resume", s.handleResume)
	mux.HandleFunc("POST /api/tasks/{id}/schedule", s.handleSchedule)
	mux.HandleFunc("POST /api/tasks/{id}/skip", s.handleSkip)
	mux.HandleFunc("DELETE /api/tasks/{id}", s.handleDelete)
	mux.HandleFunc("GET /api/tasks/{id}/file", s.handleFile)
	mux.HandleFunc("POST /api/tasks/{id}/open", s.handleOpen)
	mux.HandleFunc("POST /api/tasks/{id}/reveal", s.handleReveal)
	mux.HandleFunc("POST /api/tasks/{id}/rename", s.handleRename)
	mux.HandleFunc("POST /api/tasks/{id}/move", s.handleMove)
	mux.HandleFunc("POST /api/prompt", s.handlePrompt)
	mux.HandleFunc("GET /api/events", s.handleEvents)
	mux.HandleFunc("GET /api/prompts", s.handlePromptsPoll)
	return mux
}

// handlePromptsPoll returns and clears any queued New Download prompts. Polled by
// the single-window UI (Wails) since SSE can't stream through its asset server.
func (s *Server) handlePromptsPoll(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"prompts": s.drainPrompts()})
}

type createRequest struct {
	URL         string `json:"url"`
	FileName    string `json:"fileName,omitempty"`
	Segments    int    `json:"segments,omitempty"`
	Category    string `json:"category,omitempty"`    // IDM-style category -> folder
	Dir         string `json:"dir,omitempty"`         // explicit folder; wins over Category
	ScheduleAt  int64  `json:"scheduleAt,omitempty"`  // epoch ms; >0 holds the task until then
	Later       bool   `json:"later,omitempty"`       // hold in the "Download Later" queue (start manually)
	Description string `json:"description,omitempty"` // user note (New Download dialog)
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req createRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	dir := req.Dir
	if dir == "" && req.Category != "" {
		dir = s.eng.CategoryDir(req.Category)
	}
	view, err := s.eng.Add(req.URL, req.FileName, req.Segments, dir, epochMillis(req.ScheduleAt), req.Later)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if d := strings.TrimSpace(req.Description); d != "" {
		s.eng.SetDescription(view.ID, d)
		view.Description = d
	}
	writeJSON(w, http.StatusCreated, view)
}

// epochMillis converts an optional epoch-millisecond timestamp (0 = unset) to a
// time.Time, returning the zero time when unset.
func epochMillis(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

type categoryView struct {
	Name string `json:"name"`
	Dir  string `json:"dir"`
}

// handleCategories feeds the add dialog: ordered category->folder list plus the
// extension->category map for preselection.
func (s *Server) handleCategories(w http.ResponseWriter, r *http.Request) {
	cats := s.eng.Categories()
	seen := make(map[string]bool, len(cats))
	list := make([]categoryView, 0, len(cats))
	for _, name := range categoryOrder {
		if d, ok := cats[name]; ok {
			list = append(list, categoryView{Name: name, Dir: d})
			seen[name] = true
		}
	}
	for name, d := range cats {
		if !seen[name] {
			list = append(list, categoryView{Name: name, Dir: d})
		}
	}
	// "gui" => the server pops its OWN native top-level windows for dialogs and
	// completion (walk/webview). When false (e.g. Wails single-window), the HTML
	// renders those as in-page modals instead. Keyed off whether a native dialog
	// opener was wired, NOT cfg.GUI (Wails is a GUI but uses in-page modals).
	writeJSON(w, http.StatusOK, map[string]any{
		"categories": list, "extMap": extCategory, "ytdlp": ytdlp.Available(), "gui": s.dialog != nil})
}

// settingsView is the folder-settings payload shared by the settings endpoints.
func (s *Server) settingsView() map[string]any {
	dir, cats, custom, def, maxc := s.eng.Settings()
	return map[string]any{"downloadDir": dir, "categories": cats, "custom": custom, "defaultDir": def,
		"maxConcurrent": maxc, "shutdownWhenDone": s.eng.ShutdownWhenDone()}
}

// handleSetShutdown arms/disarms "turn off the PC when all downloads finish".
func (s *Server) handleSetShutdown(w http.ResponseWriter, r *http.Request) {
	var req struct {
		On bool `json:"on"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	s.eng.SetShutdownWhenDone(req.On)
	writeJSON(w, http.StatusOK, s.settingsView())
}

// handleSetConcurrent changes how many downloads run at once (Options panel).
func (s *Server) handleSetConcurrent(w http.ResponseWriter, r *http.Request) {
	var req struct {
		N int `json:"n"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if err := s.eng.SetMaxConcurrent(req.N); err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.settingsView())
}

func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.settingsView())
}

// handleSetDir changes the base download folder (new downloads only).
func (s *Server) handleSetDir(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Dir string `json:"dir"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if err := s.eng.SetDownloadDir(req.Dir); err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.settingsView())
}

// handleSetCategory pins a category to a remembered folder ("remember this path").
func (s *Server) handleSetCategory(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Category string `json:"category"`
		Dir      string `json:"dir"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if err := s.eng.SetCategoryDir(req.Category, req.Dir); err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.settingsView())
}

func (s *Server) handleResetPaths(w http.ResponseWriter, r *http.Request) {
	if err := s.eng.ResetPaths(); err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.settingsView())
}

// handlePickFolder shows the native folder chooser (Options "Browse"). Returns
// the chosen path, or an empty path if cancelled / no native picker is wired.
// handleQuit performs a real app exit (File → Exit) for the close-to-tray UI.
func (s *Server) handleQuit(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	if s.quit != nil {
		go s.quit() // async so this response is sent before the app tears down
	}
}

// SetUpdateSource wires the in-app updater: current is the running version and
// manifestURL is the static latest.json to check. exit fully terminates the
// process (used after the new exe is swapped in, so the relauncher can take
// over). Any empty argument disables update checks.
func (s *Server) SetUpdateSource(current, manifestURL string, exit func()) {
	s.updCurrent, s.updManifest, s.updExit = current, manifestURL, exit
}

func (s *Server) updatesEnabled() bool {
	return s.updManifest != "" && s.updCurrent != "" && s.updCurrent != "dev"
}

// handleUpdateCheck fetches latest.json and reports whether a newer build exists.
func (s *Server) handleUpdateCheck(w http.ResponseWriter, r *http.Request) {
	if !s.updatesEnabled() {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false, "current": s.updCurrent})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()
	rel, err := updater.Fetch(ctx, s.updManifest, s.eng.UserAgent())
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": true, "current": s.updCurrent, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":   true,
		"current":   s.updCurrent,
		"latest":    rel.Version,
		"available": updater.Newer(rel.Version, s.updCurrent),
		"notes":     rel.Notes,
		"date":      rel.Date,
		"mandatory": rel.Mandatory,
	})
}

// handleUpdateApply downloads the new executable, swaps it in, and schedules a
// relaunch. It returns immediately; the UI polls /api/update/status. On success
// the process exits (via s.updExit) so the detached relauncher can start the
// new build.
func (s *Server) handleUpdateApply(w http.ResponseWriter, r *http.Request) {
	if !s.updatesEnabled() {
		writeError(w, http.StatusBadRequest, "updates are not enabled for this build")
		return
	}
	s.updMu.Lock()
	if s.updApplying {
		s.updMu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"started": true, "alreadyRunning": true})
		return
	}
	s.updApplying = true
	s.updProg = updater.Progress{Phase: "checking"}
	s.updMu.Unlock()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		rel, err := updater.Fetch(ctx, s.updManifest, s.eng.UserAgent())
		if err == nil && !updater.Newer(rel.Version, s.updCurrent) {
			err = fmt.Errorf("already up to date")
		}
		if err != nil {
			s.setUpdateProgress(updater.Progress{Phase: "error", Error: err.Error()})
			s.updMu.Lock()
			s.updApplying = false
			s.updMu.Unlock()
			return
		}
		err = updater.Apply(ctx, rel, s.eng.UserAgent(), s.setUpdateProgress)
		if err != nil {
			s.setUpdateProgress(updater.Progress{Phase: "error", Error: err.Error()})
			s.updMu.Lock()
			s.updApplying = false
			s.updMu.Unlock()
			return
		}
		// New exe is in place and the relauncher is armed — persist state and exit
		// so the port frees and the relauncher starts the new build.
		s.log.Info("update applied — restarting", "to", rel.Version)
		time.Sleep(600 * time.Millisecond) // let the UI poll the "restarting" phase once
		if s.updExit != nil {
			s.updExit()
		}
	}()
	writeJSON(w, http.StatusOK, map[string]any{"started": true})
}

func (s *Server) setUpdateProgress(p updater.Progress) {
	s.updMu.Lock()
	s.updProg = p
	s.updMu.Unlock()
}

// handleUpdateStatus reports the in-flight apply progress for the UI to poll.
func (s *Server) handleUpdateStatus(w http.ResponseWriter, r *http.Request) {
	s.updMu.Lock()
	p := s.updProg
	applying := s.updApplying
	s.updMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"applying":   applying,
		"phase":      p.Phase,
		"downloaded": p.Downloaded,
		"total":      p.Total,
		"error":      p.Error,
	})
}

func (s *Server) handlePickFolder(w http.ResponseWriter, r *http.Request) {
	if s.pick == nil {
		writeJSON(w, http.StatusOK, map[string]any{"path": "", "native": false})
		return
	}
	var req struct {
		Initial string `json:"initial"`
	}
	json.NewDecoder(r.Body).Decode(&req) // body optional
	p, err := s.pick(req.Initial)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": p, "native": true})
}

// handleDoneSuppress hides the completion popup for a while (the "don't show for
// 2 hours" checkbox in the native done window). Defaults to 2h.
func (s *Server) handleDoneSuppress(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Seconds int `json:"seconds"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	d := 2 * time.Hour
	if req.Seconds > 0 {
		d = time.Duration(req.Seconds) * time.Second
	}
	s.eng.SuppressCompletionPopup(d)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleOpenDetail opens the native per-connection progress window for a task.
func (s *Server) handleOpenDetail(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ID == "" {
		writeError(w, http.StatusBadRequest, "missing id")
		return
	}
	if s.detail == nil {
		writeJSON(w, http.StatusOK, map[string]any{"native": false})
		return
	}
	if err := s.detail(req.ID); err != nil {
		s.log.Warn("failed to open detail window", "id", req.ID, "err", err)
		writeJSON(w, http.StatusOK, map[string]any{"native": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"native": true})
}

// handleOpenDone opens the native completion window for a task on demand (the
// "Preview" button). Normally the engine pops this when a download finishes.
func (s *Server) handleOpenDone(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ID == "" {
		writeError(w, http.StatusBadRequest, "missing id")
		return
	}
	if s.done == nil {
		writeJSON(w, http.StatusOK, map[string]any{"native": false})
		return
	}
	if err := s.done(req.ID); err != nil {
		s.log.Warn("failed to open completion window", "id", req.ID, "err", err)
		writeJSON(w, http.StatusOK, map[string]any{"native": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"native": true})
}

// Popup close coordination ---------------------------------------------------
//
// A download's completion ("done") and status ("detail") windows are siblings:
// closing either should close both. In the default Wails UI each popup is its
// OWN process, so they can't share an in-process handle — they coordinate
// through the server instead. A popup's Close POSTs the current wall-clock time
// here; the sibling, which polls this value, closes itself once it sees a close
// stamped AFTER it opened. Comparing client timestamps is safe because every
// popup runs on the same machine (one wall clock), and "newer than my open time"
// (rather than a bare flag) means reopening a window for the same id later isn't
// instantly dismissed by a stale signal.

func (s *Server) handlePopupClose(w http.ResponseWriter, r *http.Request) {
	var req struct {
		At int64 `json:"at"`
	}
	json.NewDecoder(r.Body).Decode(&req) // best-effort; a missing/zero "at" is a harmless no-op
	id := r.PathValue("id")
	s.closeMu.Lock()
	if req.At > s.popupCloseAt[id] {
		s.popupCloseAt[id] = req.At
	}
	at := s.popupCloseAt[id]
	s.closeMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"at": at})
}

func (s *Server) handlePopupCloseGet(w http.ResponseWriter, r *http.Request) {
	s.closeMu.Lock()
	at := s.popupCloseAt[r.PathValue("id")]
	s.closeMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"at": at})
}

// handleProbe asks yt-dlp what's downloadable at a page URL (for the overlay's
// quality dropdown).
func (s *Server) handleProbe(w http.ResponseWriter, r *http.Request) {
	u := r.URL.Query().Get("url")
	if u == "" {
		writeError(w, http.StatusBadRequest, "missing url")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	res, err := s.probes.get(ctx, u)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// handleCookies stores browser-supplied login cookies for a domain so yt-dlp can
// read login-gated media (Instagram stories/reels, etc.). The extension holds
// the live browser session and posts the cookies here; yt-dlp's own
// --cookies-from-browser can't (the running browser locks its cookie DB).
func (s *Server) handleCookies(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL     string         `json:"url"`
		Cookies []ytdlp.Cookie `json:"cookies"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if strings.TrimSpace(req.URL) == "" {
		writeError(w, http.StatusBadRequest, "missing url")
		return
	}
	if err := ytdlp.WriteCookies(req.URL, req.Cookies); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// A previously-cached anonymous failure for this site should re-probe now.
	if dom := ytdlp.RegistrableDomain(req.URL); dom != "" {
		s.probes.forget(func(u string) bool { return ytdlp.RegistrableDomain(u) == dom })
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "count": len(req.Cookies)})
}

type videoRequest struct {
	URL         string `json:"url"`
	Title       string `json:"title"`
	Selector    string `json:"selector"`
	Ext         string `json:"ext"`
	Audio       bool   `json:"audio"`
	Category    string `json:"category"`
	Dir         string `json:"dir"`
	ScheduleAt  int64  `json:"scheduleAt,omitempty"`  // epoch ms; >0 holds the task until then
	Later       bool   `json:"later,omitempty"`       // hold in the "Download Later" queue
	Description string `json:"description,omitempty"` // user note (New Download dialog)
}

// handleVideo queues a yt-dlp download chosen in the overlay.
func (s *Server) handleVideo(w http.ResponseWriter, r *http.Request) {
	var req videoRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	dir := req.Dir
	if dir == "" {
		cat := req.Category
		if cat == "" {
			cat = "Video"
		}
		dir = s.eng.CategoryDir(cat)
	}
	view, err := s.eng.AddVideo(req.URL, req.Title, req.Selector, req.Ext, dir, req.Audio, epochMillis(req.ScheduleAt), req.Later)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if d := strings.TrimSpace(req.Description); d != "" {
		s.eng.SetDescription(view.ID, d)
		view.Description = d
	}
	writeJSON(w, http.StatusCreated, view)
}

// handlePlaylistInfo reports whether ?url= is a playlist and, if so, its title
// and video count — so the UI can offer "download the whole playlist". A probe
// failure is reported as not-a-playlist so the caller falls back to single-video.
func (s *Server) handlePlaylistInfo(w http.ResponseWriter, r *http.Request) {
	u := r.URL.Query().Get("url")
	if u == "" {
		writeError(w, http.StatusBadRequest, "missing url")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	pl, err := ytdlp.ProbePlaylist(ctx, u, s.eng.UserAgent())
	if err != nil || pl == nil {
		writeJSON(w, http.StatusOK, map[string]any{"isPlaylist": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"isPlaylist": true, "title": pl.Title, "count": len(pl.Entries), "entries": pl.Entries, "mix": pl.Mix})
}

// handlePlaylistDownload queues a yt-dlp task per video (best quality, or audio).
// The UI sends the exact videos the user checked in "entries"; if absent, the
// whole playlist is enumerated and queued. Body: {url, audio, category, dir,
// later, entries:[{url,title}]}.
func (s *Server) handlePlaylistDownload(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL      string                `json:"url"`
		Audio    bool                  `json:"audio"`
		Category string                `json:"category"`
		Dir      string                `json:"dir"`
		Later    bool                  `json:"later"`
		Entries  []ytdlp.PlaylistEntry `json:"entries"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	entries := req.Entries
	title := ""
	if len(entries) == 0 { // no explicit selection — enumerate the whole playlist
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		pl, err := ytdlp.ProbePlaylist(ctx, req.URL, s.eng.UserAgent())
		if err != nil {
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
		if pl == nil {
			writeError(w, http.StatusBadRequest, "not a playlist")
			return
		}
		entries, title = pl.Entries, pl.Title
	}
	dir := req.Dir
	if dir == "" {
		cat := req.Category
		if cat == "" {
			cat = "Video"
		}
		dir = s.eng.CategoryDir(cat)
	}
	ext := "mp4"
	if req.Audio {
		ext = "mp3"
	}
	added := 0
	for _, e := range entries {
		if strings.TrimSpace(e.URL) == "" {
			continue
		}
		if _, err := s.eng.AddVideo(e.URL, e.Title, "", ext, dir, req.Audio, time.Time{}, req.Later); err == nil {
			added++
		}
	}
	s.log.Info("playlist queued", "videos", added, "later", req.Later)
	writeJSON(w, http.StatusOK, map[string]any{"count": added, "title": title})
}

// handleSkip marks or clears a Download Later task's "skip" flag (excluded from
// Start All). Body {skip: true|false}.
func (s *Server) handleSkip(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Skip bool `json:"skip"`
	}
	json.NewDecoder(r.Body).Decode(&req) // missing => false (un-skip)
	if err := s.eng.SetSkip(r.PathValue("id"), req.Skip); err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleSchedule sets or clears a task's scheduled start. Body {at: <epoch ms>}:
// a future time holds the task until then; 0 or omitted cancels the schedule.
func (s *Server) handleSchedule(w http.ResponseWriter, r *http.Request) {
	var req struct {
		At int64 `json:"at"`
	}
	json.NewDecoder(r.Body).Decode(&req) // a missing/zero "at" cancels the schedule
	id := r.PathValue("id")
	var err error
	if req.At <= 0 {
		err = s.eng.Unschedule(id)
	} else {
		err = s.eng.Schedule(id, time.UnixMilli(req.At))
	}
	if err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"tasks": s.eng.List()})
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	view, err := s.eng.Get(r.PathValue("id"))
	if err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) handlePause(w http.ResponseWriter, r *http.Request) {
	if err := s.eng.Pause(r.PathValue("id")); err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleResume(w http.ResponseWriter, r *http.Request) {
	if err := s.eng.Resume(r.PathValue("id")); err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	removeFile := r.URL.Query().Get("file") == "1"
	if err := s.eng.Delete(r.PathValue("id"), removeFile); err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleFile streams a completed download back through the browser
// (equivalent of the reference repo's GetDownloadTaskFile).
func (s *Server) handleFile(w http.ResponseWriter, r *http.Request) {
	path, err := s.eng.FilePath(r.PathValue("id"))
	if err != nil {
		writeEngineError(w, err)
		return
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filepath.Base(path)))
	http.ServeFile(w, r, path)
}

type renameRequest struct {
	Name string `json:"name"`
}

func (s *Server) handleRename(w http.ResponseWriter, r *http.Request) {
	var req renameRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	view, err := s.eng.RenameFile(r.PathValue("id"), req.Name)
	if err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

type moveRequest struct {
	Dir string `json:"dir"`
}

func (s *Server) handleMove(w http.ResponseWriter, r *http.Request) {
	var req moveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	view, err := s.eng.MoveFile(r.PathValue("id"), req.Dir)
	if err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

type promptRequest struct {
	URL      string `json:"url"`
	Name     string `json:"name,omitempty"`
	Video    bool   `json:"video,omitempty"`    // true => the dialog's Start posts /api/video, not /api/tasks
	Selector string `json:"selector,omitempty"` // yt-dlp format selector
	Ext      string `json:"ext,omitempty"`      // expected container (mp4 / mp3)
	Audio    bool   `json:"audio,omitempty"`    // audio-only
	Title    string `json:"title,omitempty"`
}

// handlePrompt opens the native "New Download" window for url (GUI mode). The
// response's "native" flag tells the caller (e.g. the browser extension)
// whether MyIDM handled it or they should fall back to their own popup.
func (s *Server) handlePrompt(w http.ResponseWriter, r *http.Request) {
	var req promptRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if strings.TrimSpace(req.URL) == "" {
		writeError(w, http.StatusBadRequest, "missing url")
		return
	}
	if s.dialog == nil && !s.webMode {
		writeJSON(w, http.StatusOK, map[string]any{"native": false})
		return
	}
	q := url.Values{}
	q.Set("url", req.URL)
	if req.Name != "" {
		q.Set("name", req.Name)
	}
	if req.Video { // carry the chosen yt-dlp format through to the dialog
		q.Set("video", "1")
		q.Set("selector", req.Selector)
		q.Set("vext", req.Ext)
		if req.Audio {
			q.Set("audio", "1")
		}
		if req.Title != "" {
			q.Set("title", req.Title)
		}
	}
	if s.dialog != nil { // native window (walk/webview)
		if err := s.dialog(q); err != nil {
			s.log.Warn("failed to open native download dialog", "err", err)
			writeJSON(w, http.StatusOK, map[string]any{"native": false})
			return
		}
	} else { // Wails single-window: relay to the page over SSE
		q.Set("dialog", "1")
		s.queuePrompt(q.Encode())
	}
	writeJSON(w, http.StatusOK, map[string]any{"native": true})
}

// handleOpen launches the completed file with the OS default application
// (the browser can't open a local file directly).
func (s *Server) handleOpen(w http.ResponseWriter, r *http.Request) {
	path, err := s.eng.FilePath(r.PathValue("id"))
	if err != nil {
		writeEngineError(w, err)
		return
	}
	if _, err := os.Stat(path); err != nil {
		writeError(w, http.StatusNotFound, "file is no longer on disk")
		return
	}
	if err := openWithDefaultApp(path); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// openWithDefaultApp opens path in whatever application the OS associates with
// its type. On Windows we route through PowerShell's Start-Process with a
// single-quoted -LiteralPath so spaces, Unicode, and shell metacharacters
// (&, %, parentheses) in the filename are handled safely.
func openWithDefaultApp(path string) error {
	switch runtime.GOOS {
	case "windows":
		// Invoke-Item — NOT Start-Process, which has no -LiteralPath parameter and
		// so errored on every open (the file never launched). Invoke-Item opens the
		// file with its default app exactly like a double-click; -LiteralPath takes
		// the path verbatim so spaces, Unicode, and []/&/% in the name are safe.
		script := "Invoke-Item -LiteralPath '" + strings.ReplaceAll(path, "'", "''") + "'"
		cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", script)
		procutil.Hidden(cmd)
		return cmd.Start()
	case "darwin":
		return exec.Command("open", path).Start()
	default:
		return exec.Command("xdg-open", path).Start()
	}
}

// handleReveal opens Explorer with the file selected. Local-app nicety.
func (s *Server) handleReveal(w http.ResponseWriter, r *http.Request) {
	path, err := s.eng.FilePath(r.PathValue("id"))
	if err != nil {
		writeEngineError(w, err)
		return
	}
	if runtime.GOOS == "windows" {
		// explorer exits non-zero even on success; fire and forget.
		cmd := exec.Command("explorer.exe", "/select,", filepath.Clean(path))
		procutil.Hidden(cmd)
		cmd.Start()
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleEvents pushes a full task snapshot every 500ms over SSE.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	send := func() bool {
		// Relay any queued New Download prompts (from the extension) as a named
		// SSE event so the single-window UI can pop the in-page dialog.
		if prompts := s.drainPrompts(); len(prompts) > 0 {
			if pb, err := json.Marshal(prompts); err == nil {
				if _, err := fmt.Fprintf(w, "event: prompt\ndata: %s\n\n", pb); err != nil {
					return false
				}
			}
		}
		b, err := json.Marshal(map[string]any{"tasks": s.eng.List()})
		if err != nil {
			return false
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	if !send() {
		return
	}
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			if !send() {
				return
			}
		}
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func writeEngineError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, engine.ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, engine.ErrConflict):
		writeError(w, http.StatusConflict, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}
