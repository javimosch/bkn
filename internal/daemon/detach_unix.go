//go:build !windows

package daemon

import (
	"os/exec"
	"syscall"
)

// detach puts the child in its own session so it outlives this process and
// ignores the invoking terminal's signals.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
