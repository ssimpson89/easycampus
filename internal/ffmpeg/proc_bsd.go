//go:build !windows && !linux

package ffmpeg

import (
	"os/exec"
	"syscall"
)

// setSysProcAttr puts ffmpeg into its own process group so we can signal
// the group (and reap helper children) deterministically. The child of
// a process running as PID N appears in process group N after Setpgid;
// kill(-N, SIGTERM) then signals every process in that group. (macOS has
// no Pdeathsig equivalent; the pid-file reaper covers orphans.)
func setSysProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}
