//go:build !windows

package tunnel

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// setProcessGroup puts cloudflared in its own process group, so a Ctrl+C in the
// terminal reaches hooktape only and shutdown stays ours to order.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func terminate(cmd *exec.Cmd) error {
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
}

// kill signals the whole group, since cloudflared may have children of its own.
// Once the child is reaped its pid can be reused, so that group is not ours.
func kill(cmd *exec.Cmd) error {
	if errors.Is(cmd.Process.Signal(syscall.Signal(0)), os.ErrProcessDone) {
		return nil
	}

	return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
