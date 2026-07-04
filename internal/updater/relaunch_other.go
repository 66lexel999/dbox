//go:build !windows

package updater

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

// relaunch spawns a detached shell that waits for this process (by PID) to exit,
// then execs the new binary with the same args. Setsid detaches it so it
// survives our exit.
func relaunch(exe string, args []string) error {
	cmdline := shSingleQuote(exe)
	for _, a := range args {
		cmdline += " " + shSingleQuote(a)
	}
	script := fmt.Sprintf("while kill -0 %d 2>/dev/null; do sleep 0.5; done; sleep 0.4; exec %s",
		os.Getpid(), cmdline)
	cmd := exec.Command("sh", "-c", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd.Start()
}

func shSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
