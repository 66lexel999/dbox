//go:build windows

// Package wailsui hosts MyIDM's window with Wails v2. Instead of embedding a
// separate frontend, it serves the existing HTTP handler (the IDM-skinned HTML
// UI + JSON API) straight through Wails' AssetServer — so the whole proven web
// UI is reused unchanged, and because it renders in WebView2 there is no native
// ListView to flicker (the problem that plagued the lxn/walk front end).
package wailsui

import (
	"context"
	"log/slog"
	"net/http"
	"sync"

	"myidm/internal/gui"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/windows"
	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// the live window context, captured at startup so Activate can reach the runtime.
var (
	ctxMu      sync.Mutex
	winCtx     context.Context
	reallyQuit bool // set by Quit() so OnBeforeClose lets the window actually close
	trayOK     bool // tray icon is up → close hides to it; else close just minimizes
)

// Run opens the window and blocks on the Wails event loop until it closes.
// handler serves both the UI (GET /) and the JSON API (/api/*). onClose runs on
// shutdown (engine + server teardown).
func Run(handler http.Handler, log *slog.Logger, onClose func()) error {
	return wails.Run(&options.App{
		Title:     "D BOX — Download Manager",
		Width:     763, // default (your current size ÷ 1.39 DPI); overridden by the remembered size
		Height:    343,
		MinWidth:  500,
		MinHeight: 280,
		// Assets nil + Handler set => every request (GET UI + POST/DELETE API)
		// is served by our existing mux.
		AssetServer:      &assetserver.Options{Handler: handler},
		BackgroundColour: &options.RGBA{R: 0x30, G: 0x33, B: 0x36, A: 255}, // --panel, avoids white flash
		OnStartup: func(ctx context.Context) {
			ctxMu.Lock()
			winCtx = ctx
			ctxMu.Unlock()
			if w, h, ok := gui.LoadWinSize("main"); ok {
				wruntime.WindowSetSize(ctx, w, h) // restore the user's last window size
			}
			wruntime.WindowCenter(ctx) // center at the restored (or default) size
			// IDM-style tray: closing the window then hides to the notification area
			// instead of quitting (see OnBeforeClose). Left-click / "Open D BOX"
			// restores it; "Exit" really quits.
			ready := make(chan bool, 1)
			go gui.RunTray("D BOX — Download Manager", Activate, Quit, ready)
			ok := <-ready
			ctxMu.Lock()
			trayOK = ok
			ctxMu.Unlock()
		},
		// Close (the X) hides to tray and keeps downloading in the background,
		// rather than quitting — only a real Exit (tray menu / File → Exit) closes.
		OnBeforeClose: func(ctx context.Context) bool {
			if w, h := wruntime.WindowGetSize(ctx); w > 0 && h > 0 {
				gui.SaveWinSize("main", w, h) // remember size on every close (hide-to-tray and real exit)
			}
			ctxMu.Lock()
			rq, hasTray := reallyQuit, trayOK
			ctxMu.Unlock()
			if rq {
				return false // allow the close
			}
			if hasTray {
				wruntime.WindowHide(ctx) // hide to tray (taskbar button gone, like IDM)
			} else {
				wruntime.WindowMinimise(ctx) // no tray: keep running, restorable from taskbar
			}
			return true // prevent the close
		},
		OnShutdown: func(_ context.Context) {
			gui.RemoveTray()
			if onClose != nil {
				onClose()
			}
		},
		Windows: &windows.Options{
			Theme:                windows.Dark, // dark title bar / frame
			WebviewIsTransparent: false,
		},
	})
}

// Quit requests a real application exit (the tray's "Exit", or File → Exit in the
// UI via POST /api/quit). It removes the tray icon and tells Wails to close;
// OnBeforeClose then lets the window through because reallyQuit is set.
func Quit() {
	ctxMu.Lock()
	reallyQuit = true
	ctx := winCtx
	ctxMu.Unlock()
	gui.RemoveTray()
	if ctx != nil {
		wruntime.Quit(ctx)
	}
}

// Activate brings the window to the foreground. Called when the browser
// extension captures a download (POST /api/prompt) so the in-page New Download
// dialog surfaces over the browser instead of only flashing in the taskbar.
// Syncs Wails' own state, then uses the Win32 AttachThreadInput trick to win the
// foreground from a background thread (Wails' WindowShow alone loses the race).
func Activate() {
	ctxMu.Lock()
	ctx := winCtx
	ctxMu.Unlock()
	if ctx != nil {
		wruntime.WindowUnminimise(ctx)
		wruntime.WindowShow(ctx)
		wruntime.WindowCenter(ctx) // re-center every time it reappears (e.g. from the tray)
	}
	forceForeground()
}
