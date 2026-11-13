//go:build !linux

package ingest

import "os/exec"

// bindToParent: on other platforms FFmpeg exits on the broken stdout pipe.
// Windows job objects are added with the native service (planned).
func bindToParent(*exec.Cmd) {}
