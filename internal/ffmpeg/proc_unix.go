//go:build !windows

package ffmpeg

import (
	"os/exec"
	"syscall"
)

// setSysProcAttr puts ffmpeg into its own process group so we can signal
// the group (and reap helper children) deterministically. The child of
// a process running as PID N appears in process group N after Setpgid;
// kill(-N, SIGTERM) then signals every process in that group.
func setSysProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func sendSIGTERM(pid int) error {
	if pid <= 0 {
		return nil
	}
	if err := syscall.Kill(-pid, syscall.SIGTERM); err == nil {
		return nil
	}
	return syscall.Kill(pid, syscall.SIGTERM)
}

func sendSIGKILL(pid int, _ *exec.Cmd) error {
	if pid <= 0 {
		return nil
	}
	if err := syscall.Kill(-pid, syscall.SIGKILL); err == nil {
		return nil
	}
	return syscall.Kill(pid, syscall.SIGKILL)
}
