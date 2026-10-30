//go:build !windows

package tunnel

import (
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
func kill(cmd *exec.Cmd) error {
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
