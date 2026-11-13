//go:build linux

package ingest

import (
	"os/exec"
	"syscall"
)

// bindToParent kills FFmpeg if the recorder dies abruptly (kill -9, crash),
// so no orphan keeps a camera session open.
func bindToParent(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}
