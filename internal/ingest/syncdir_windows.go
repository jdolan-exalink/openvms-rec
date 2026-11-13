//go:build windows

package ingest

// syncDir is a no-op on Windows: NTFS journals metadata and directories
// cannot be opened for FlushFileBuffers without backup privileges.
func syncDir(string) {}
