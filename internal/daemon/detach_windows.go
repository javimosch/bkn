//go:build windows

package daemon

import "os/exec"

func detach(cmd *exec.Cmd) {}
