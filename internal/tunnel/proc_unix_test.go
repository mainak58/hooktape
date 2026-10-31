//go:build !windows

package tunnel

import (
	"os/exec"
	"testing"
)

func TestKillSkipsReapedProcess(t *testing.T) {
	cmd := exec.Command("true")
	setProcessGroup(cmd)
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}

	if err := kill(cmd); err != nil {
		t.Errorf("kill = %v, want nil for a process already gone", err)
	}
}
