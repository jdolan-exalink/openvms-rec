//go:build !linux && !windows

package platform

func usage(path string) (DiskUsage, error) { return DiskUsage{}, ErrUnsupported }

func volume(path string) (VolumeInfo, error) { return VolumeInfo{}, ErrUnsupported }
