//go:build windows

package terminal

import (
	"os/exec"
	"strconv"
)

// setProcessGroup makes cancellation kill the shell's whole process tree;
// killing bash alone would orphan its children on Windows.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.Cancel = func() error { return killTree(cmd) }
}

func killTree(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	kill := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid))
	if err := kill.Run(); err != nil {
		return cmd.Process.Kill()
	}
	return nil
}
