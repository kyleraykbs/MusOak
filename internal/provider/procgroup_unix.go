//go:build unix

package provider

import (
	"os/exec"
	"syscall"
)

// killGroupOnCancel starts cmd in a process group of its own and makes a
// cancelled context kill the whole group, so the processes cmd started die with
// it instead of carrying on with its pipes open.
func killGroupOnCancel(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// A negative pid names the group.
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
}
