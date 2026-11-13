// Package platform hides OS-specific disk and volume queries behind a small,
// portable API. Linux and Windows implementations live in build-tagged files;
// other platforms get a conservative fallback that reports what it can.
package platform

import (
	"errors"
	"fmt"
)

// ErrUnsupported is returned when the running OS cannot answer a query.
var ErrUnsupported = errors.New("platform: operation not supported on this OS")

// DiskUsage is a point-in-time view of a filesystem, in bytes.
type DiskUsage struct {
	// TotalBytes is the filesystem size as reported by the OS.
	TotalBytes uint64
	// FreeBytes is the space the service account can actually allocate
	// (statfs f_bavail / GetDiskFreeSpaceEx lpFreeBytesAvailable). It already
	// excludes root-reserved blocks and per-user NTFS quotas.
	FreeBytes uint64
	// ReadOnly reports a read-only mount when the OS exposes it.
	ReadOnly bool
}

// VolumeInfo identifies the physical/logical volume that holds a path.
type VolumeInfo struct {
	// StableID is the identifier used to detect the same volume behind
	// different paths or drive letters: a filesystem UUID or device number on
	// Linux, the volume GUID path on Windows.
	StableID string
	// MountPoint is the root of the mounted filesystem (e.g. "/srv/cctv1" or "D:\").
	MountPoint string
	// Device is the backing device or volume name when known.
	Device string
	// FSType is the filesystem name (ext4, xfs, NTFS, ReFS, ...).
	FSType string
	// IsSystem is true when the volume also holds the OS.
	IsSystem bool
	// IsNetwork is true for NFS/SMB/remote drives (not suitable for continuous capture).
	IsNetwork bool
	// IsRemovable is true for removable drives when the OS exposes it.
	IsRemovable bool
}

// Usage returns disk usage for the filesystem containing path.
func Usage(path string) (DiskUsage, error) {
	u, err := usage(path)
	if err != nil {
		return DiskUsage{}, fmt.Errorf("disk usage for %q: %w", path, err)
	}
	return u, nil
}

// Volume returns the identity of the volume containing path.
func Volume(path string) (VolumeInfo, error) {
	v, err := volume(path)
	if err != nil {
		return VolumeInfo{}, fmt.Errorf("volume info for %q: %w", path, err)
	}
	return v, nil
}
