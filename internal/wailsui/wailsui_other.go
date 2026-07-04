//go:build !windows

package wailsui

import (
	"errors"
	"log/slog"
	"net/http"
)

// Run is unavailable off Windows (Wails/WebView2 is Windows-only here).
func Run(_ http.Handler, _ *slog.Logger, _ func()) error {
	return errors.New("wails UI is only supported on windows")
}

// Activate is a no-op off Windows.
func Activate() {}
