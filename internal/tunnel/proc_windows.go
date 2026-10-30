//go:build windows

package tunnel

import "os/exec"

// Windows has no process groups to inherit a Ctrl+C, so there is nothing to set.
func setProcessGroup(cmd *exec.Cmd) {}

func terminate(cmd *exec.Cmd) error {
	return cmd.Process.Kill()
}

func kill(cmd *exec.Cmd) error {
	return cmd.Process.Kill()
}
