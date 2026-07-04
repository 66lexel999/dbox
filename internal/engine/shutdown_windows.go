//go:build windows

package engine

import (
	"os/exec"

	"myidm/internal/procutil"
)

// systemShutdown asks Windows to power off after a 60-second grace period, which
// pops the standard shutdown warning the user can abort with `shutdown /a` (or by
// un-checking the option, which calls cancelSystemShutdown).
func systemShutdown() error {
	cmd := exec.Command("shutdown", "/s", "/t", "60",
		"/c", "D BOX: all downloads finished. Shutting down in 60 seconds — run 'shutdown /a' to cancel.")
	procutil.Hidden(cmd)
	return cmd.Run()
}

// cancelSystemShutdown aborts a pending shutdown scheduled by systemShutdown.
func cancelSystemShutdown() error {
	cmd := exec.Command("shutdown", "/a")
	procutil.Hidden(cmd)
	return cmd.Run() // errors when nothing is scheduled — caller ignores it
}
