//go:build !unix

package provider

import "os/exec"

// killGroupOnCancel is the plain kill where there are no process groups to
// name: the command itself is stopped, and WaitDelay bounds what it leaves.
func killGroupOnCancel(*exec.Cmd) {}
