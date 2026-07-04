//go:build windows

// Package procutil centralizes OS-process tweaks shared across MyIDM. On
// Windows, a GUI-subsystem process (built with -H windowsgui) has no console to
// hand down, so every console child it launches — yt-dlp.exe, ffmpeg, the
// PowerShell helpers — would otherwise pop its own cmd window. Hidden suppresses
// that for any command we spawn.
package procutil

import (
	"os/exec"
	"syscall"
)

// CREATE_NO_WINDOW: start the child without allocating a console window.
const createNoWindow = 0x08000000

// Hidden makes a spawned command run without flashing a console window.
//
// It sets ONLY CreateNoWindow — deliberately NOT SysProcAttr.HideWindow. The
// HideWindow flag adds STARTF_USESHOWWINDOW|SW_HIDE to the child's STARTUPINFO,
// which makes that process's windows start hidden. For a console tool that's
// harmless, but for a child that hosts a GUI (a WebView2 popup, or the
// PowerShell folder-browser dialog) it leaves the WebView2 surface uncomposited
// — the frame shows but the content is blank (document.visibilityState reports
// "hidden"). CreateNoWindow alone suppresses the console for console children
// and is simply ignored for GUI children, so it never hides a window we want.
func Hidden(c *exec.Cmd) {
	if c == nil {
		return
	}
	if c.SysProcAttr == nil {
		c.SysProcAttr = &syscall.SysProcAttr{}
	}
	c.SysProcAttr.CreationFlags |= createNoWindow
}
