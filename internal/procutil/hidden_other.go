//go:build !windows

package procutil

import "os/exec"

// Hidden is a no-op off Windows: console suppression is a Win32 concept and
// other platforms don't allocate a console for child processes.
func Hidden(c *exec.Cmd) {}
