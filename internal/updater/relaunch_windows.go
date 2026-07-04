//go:build windows

package updater

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

// CREATE_NO_WINDOW runs the helper with no console window; CREATE_NEW_PROCESS_GROUP
// detaches it from our Ctrl-C group so it survives our exit. NOTE: do NOT also
// pass DETACHED_PROCESS — Win32 treats DETACHED_PROCESS and CREATE_NO_WINDOW as
// mutually exclusive, and combining them makes the helper's own Start-Process
// child-creation misbehave (the relaunch silently no-ops).
const (
	createNewProcessGroup = 0x00000200
	createNoWindow        = 0x08000000
)

// relaunch spawns a detached PowerShell that waits for THIS process to exit
// (freeing :8081 and unlocking the old exe fully), pauses briefly, then starts
// the freshly-swapped executable with the same args. Wait-Process keys on the
// exact PID, so it's immune to locale and to memory-column digit coincidences
// that break tasklist/find text matching. The child outlives us (detached).
func relaunch(exe string, args []string) error {
	start := "Start-Process -FilePath " + psSingleQuote(exe)
	if len(args) > 0 {
		quoted := make([]string, len(args))
		for i, a := range args {
			quoted[i] = psSingleQuote(a)
		}
		start += " -ArgumentList @(" + strings.Join(quoted, ",") + ")"
	}
	ps := fmt.Sprintf("Wait-Process -Id %d -ErrorAction SilentlyContinue; Start-Sleep -Milliseconds 400; %s",
		os.Getpid(), start)
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-WindowStyle", "Hidden", "-Command", ps)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: createNewProcessGroup | createNoWindow,
	}
	return cmd.Start()
}

// psSingleQuote wraps s for a PowerShell single-quoted string (doubling any ').
func psSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
