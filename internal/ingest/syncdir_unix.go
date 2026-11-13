//go:build !windows

package ingest

import "os"

// syncDir fsyncs a directory so a rename survives power loss (POSIX).
func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
}
