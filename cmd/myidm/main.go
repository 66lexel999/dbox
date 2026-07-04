// D BOX — a self-contained segmented download manager with a native desktop UI.
//
// Build:  go build -tags "desktop production" -ldflags "-H windowsgui" -o bin/DBox.exe ./cmd/myidm
//
//	(-H windowsgui launches the app with no console/cmd window)
//
// Run:    DBox.exe                       open the native WebView2 window (default)
//
//	DBox.exe -gui=gio              native pure-Go (Gio) window
//	DBox.exe -gui=off -open        headless server + browser UI
//	DBox.exe -dialog -url <u>      internal: native "New Download" window
//
// Architecture borrowed from github.com/maxuanquang/idm (handler -> logic ->
// dataaccess), collapsed into one binary: JSON store instead of MySQL,
// in-process queue instead of Kafka, direct-to-disk instead of MinIO, and a
// WebView2 window instead of a browser tab.
package main

import (
	"context"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"time"

	"myidm/internal/config"
	"myidm/internal/engine"
	"myidm/internal/gui"
	"myidm/internal/procutil"
	"myidm/internal/server"
	"myidm/internal/store"
	"myidm/internal/updater"
	"myidm/internal/version"
	"myidm/internal/wailsui"
	"myidm/internal/walkui"
)

func main() {
	gui.EnableDPIAwareness() // make every window render sharp (before any window is created)

	// Secondary "window" modes are lightweight processes that open a single
	// native window against an already-running MyIDM server.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "-dialog":
			exitOn("dialog", runDialog(os.Args[2:]))
			return
		case "-done":
			exitOn("done", runWindow(os.Args[2:], gui.RunDone))
			return
		case "-detail":
			exitOn("detail", runWindow(os.Args[2:], gui.RunDetail))
			return
		}
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

func exitOn(tag string, err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, tag+":", err)
		os.Exit(1)
	}
}

func runDialog(args []string) error {
	fs := flag.NewFlagSet("dialog", flag.ContinueOnError)
	listen := fs.String("listen", "127.0.0.1:8081", "MyIDM server address")
	q := fs.String("q", "", "base64url-encoded query for the New Download page")
	target := fs.String("url", "", "download URL (legacy; prefer -q)")
	name := fs.String("name", "", "suggested file name (legacy; prefer -q)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var query string
	switch {
	case *q != "":
		b, err := base64.RawURLEncoding.DecodeString(*q)
		if err != nil {
			return fmt.Errorf("bad -q: %w", err)
		}
		query = string(b)
	case *target != "":
		v := url.Values{}
		v.Set("url", *target)
		if *name != "" {
			v.Set("name", *name)
		}
		query = v.Encode()
	default:
		return errors.New("missing -q or -url")
	}
	return gui.RunDialog("http://"+*listen, query)
}

// runWindow drives the -done / -detail window modes (both take -listen + -id).
func runWindow(args []string, fn func(serverURL, id string) error) error {
	fs := flag.NewFlagSet("window", flag.ContinueOnError)
	listen := fs.String("listen", "127.0.0.1:8081", "MyIDM server address")
	id := fs.String("id", "", "task id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *id == "" {
		return errors.New("missing -id")
	}
	return fn("http://"+*listen, *id)
}

func run() error {
	updater.CleanupOld() // remove the <exe>.old left by a previous self-update

	cfg, err := config.FromFlags(os.Args[1:])
	if err != nil {
		return err
	}
	// One-time MyIDM -> flowerX move, BEFORE the new dirs are created (the absence
	// of the new dir is what triggers the rename of the old one).
	migrationNotes := migrateFromMyIDM(cfg)
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}
	if err := os.MkdirAll(cfg.DownloadDir, 0o755); err != nil {
		return fmt.Errorf("create download dir: %w", err)
	}

	log := newLogger(cfg.DataDir)
	for _, n := range migrationNotes {
		log.Info("flowerX migration", "detail", n)
	}

	st, err := store.New(cfg.DataDir)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	eng := engine.New(cfg, st, log)
	if err := eng.Start(ctx); err != nil {
		return err
	}

	s := server.New(eng, log)

	serverURL := "http://" + cfg.Listen

	// The Options panel's folder "Browse" uses the native chooser in any GUI mode.
	if cfg.GUI {
		s.SetFolderPicker(func(initial string) (string, error) { return gui.PickFolder(initial), nil })
	}
	// Real per-file Windows shell icons in the downloads table.
	s.SetIconResolver(gui.FileIconPNGEx, gui.DefaultAppIsGeneric)

	// Secondary windows (New Download / status / completion) open IN-PROCESS,
	// native to whichever main UI is running — Gio windows for the Gio UI, in-
	// process WebView2 windows for the WebView2 UI. No separate processes, no cold
	// start, and the main process coordinates them (completion popup closes status).
	switch cfg.UIKind {
	case "wails":
		// Classic-IDM separate floating windows: the New Download / status /
		// completion dialogs each run as their own lightweight DBox.exe process
		// (-dialog/-detail/-done), fully isolated from the Wails webview, talking
		// to this server over :8081. Each self-raises to the foreground over the
		// browser; AllowForeground lets the freshly-spawned child steal focus.
		exe, _ := os.Executable()
		spawn := func(args ...string) {
			gui.AllowForeground()
			c := exec.Command(exe, append(args, "-listen", cfg.Listen)...)
			procutil.Hidden(c)
			if err := c.Start(); err != nil {
				log.Warn("popup spawn failed", "args", args, "err", err)
			}
		}
		s.SetDialogOpener(func(q url.Values) error {
			spawn("-dialog", "-q", base64.RawURLEncoding.EncodeToString([]byte(q.Encode())))
			return nil
		})
		s.SetDetailOpener(func(id string) error { spawn("-detail", "-id", id); return nil })
		s.SetDoneOpener(func(id string) error { spawn("-done", "-id", id); return nil })
		s.SetQuit(wailsui.Quit) // File → Exit really quits (the X closes to tray)
		eng.SetCompletionNotifier(func(id string) { spawn("-done", "-id", id) })
	case "walk":
		s.SetDialogOpener(func(q url.Values) error { walkui.OpenDialog(eng, log, q); return nil })
		s.SetDetailOpener(func(id string) error { walkui.OpenDetail(eng, log, id); return nil })
		s.SetDoneOpener(func(id string) error { walkui.OpenDone(eng, log, id); return nil })
		eng.SetCompletionNotifier(func(id string) { walkui.OpenDone(eng, log, id) })
	case "webview":
		gui.SetSharedWebviewData(filepath.Join(cfg.DataDir, "webview"))
		s.SetDialogOpener(func(q url.Values) error { gui.OpenDialog(serverURL, q.Encode()); return nil })
		s.SetDetailOpener(func(id string) error { gui.OpenDetail(serverURL, id); return nil })
		s.SetDoneOpener(func(id string) error { gui.OpenDone(serverURL, id); return nil })
		eng.SetCompletionNotifier(func(id string) { gui.OpenDone(serverURL, id) })
	}

	srv := &http.Server{Addr: cfg.Listen, Handler: s.Handler()}
	errCh := make(chan error, 1)
	go func() {
		log.Info("MyIDM listening", "url", "http://"+cfg.Listen, "downloads", cfg.DownloadDir, "ui", cfg.UIKind)
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	shutdown := func() {
		log.Info("shutting down")
		eng.Shutdown(5 * time.Second)
		sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
	}

	// In-app self-update: check version.ManifestURL for a newer build and, on the
	// user's click, swap this exe and relaunch. updExit fully terminates the
	// process (works in every UI mode) so the detached relauncher can start the
	// new build; a clean shutdown() first persists tasks and frees the port.
	s.SetUpdateSource(version.Version, version.ManifestURL, func() {
		shutdown()
		os.Exit(0)
	})

	// Let ListenAndServe fail fast (e.g. port in use) before opening a window.
	serverFailed := func() error {
		select {
		case err := <-errCh:
			return err
		case <-time.After(250 * time.Millisecond):
			return nil
		}
	}

	var runErr error
	switch cfg.UIKind {
	case "wails":
		// No native dialog/detail/completion openers are wired (handled above):
		// the HTML UI renders those as in-page modals. Wails just hosts the window
		// and serves the existing handler — no native ListView, so no flicker.
		if runErr = serverFailed(); runErr == nil {
			if guiErr := wailsui.Run(s.Handler(), log, nil); guiErr != nil {
				log.Warn("wails window unavailable; serving in the browser instead", "err", guiErr)
				go openBrowser("http://" + cfg.Listen)
				select {
				case runErr = <-errCh:
				case <-ctx.Done():
				}
			}
		}
	case "walk":
		if runErr = serverFailed(); runErr == nil {
			a := walkui.New(eng, log, func(u string) {
				q := url.Values{}
				q.Set("url", u)
				walkui.OpenDialog(eng, log, q)
			}, func(id string) { walkui.OpenDetail(eng, log, id) })
			walkui.RunBlocking(a, shutdown) // blocks on the walk message loop; on close -> shutdown + exit
		}
	case "webview":
		if runErr = serverFailed(); runErr == nil {
			if guiErr := gui.RunMain("http://"+cfg.Listen, cfg.DataDir, nil); guiErr != nil {
				log.Warn("native window unavailable; serving in the browser instead", "err", guiErr)
				go openBrowser("http://" + cfg.Listen)
				select {
				case runErr = <-errCh:
				case <-ctx.Done():
				}
			}
		}
	default: // off / headless
		if cfg.OpenBrowser {
			go openBrowser("http://" + cfg.Listen)
		}
		select {
		case runErr = <-errCh:
		case <-ctx.Done():
		}
	}

	shutdown()
	return runErr
}

// newLogger writes to both stderr and <dataDir>/myidm.log, so logs survive a
// console-free ("-H windowsgui") GUI build.
func newLogger(dataDir string) *slog.Logger {
	var out io.Writer = os.Stderr
	if f, err := os.OpenFile(filepath.Join(dataDir, "myidm.log"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
		out = io.MultiWriter(os.Stderr, f)
	}
	return slog.New(slog.NewTextHandler(out, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

func openBrowser(url string) {
	time.Sleep(300 * time.Millisecond) // let the listener come up first
	switch runtime.GOOS {
	case "windows":
		exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	case "darwin":
		exec.Command("open", url).Start()
	default:
		exec.Command("xdg-open", url).Start()
	}
}
