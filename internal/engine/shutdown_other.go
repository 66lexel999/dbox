//go:build !windows

package engine

import "os/exec"

// systemShutdown powers off on Unix-likes (best-effort; needs privileges).
func systemShutdown() error {
	return exec.Command("shutdown", "-h", "+1").Run()
}

// cancelSystemShutdown aborts a pending shutdown (`shutdown -c`).
func cancelSystemShutdown() error {
	return exec.Command("shutdown", "-c").Run()
}
