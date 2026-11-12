//go:build windows

package platform

import (
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/windows"
)

func usage(path string) (DiskUsage, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return DiskUsage{}, err
	}
	var freeAvail, total, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(p, &freeAvail, &total, &totalFree); err != nil {
		return DiskUsage{}, err
	}
	u := DiskUsage{TotalBytes: total, FreeBytes: freeAvail}
	if root, err := volumePathName(path); err == nil {
		if _, _, flags, err := volumeInformation(root); err == nil {
			u.ReadOnly = flags&windows.FILE_READ_ONLY_VOLUME != 0
		}
	}
	return u, nil
}

func volume(path string) (VolumeInfo, error) {
	root, err := volumePathName(path)
	if err != nil {
		return VolumeInfo{}, err
	}
	info := VolumeInfo{MountPoint: root}

	rootPtr, err := windows.UTF16PtrFromString(root)
	if err != nil {
		return VolumeInfo{}, err
	}
	// \\?\Volume{GUID}\ is stable across drive-letter changes and mount points.
	guid := make([]uint16, windows.MAX_PATH)
	if err := windows.GetVolumeNameForVolumeMountPoint(rootPtr, &guid[0], uint32(len(guid))); err == nil {
		info.StableID = "guid:" + strings.ToLower(windows.UTF16ToString(guid))
		info.Device = windows.UTF16ToString(guid)
	}
	if fs, serial, _, err := volumeInformation(root); err == nil {
		info.FSType = fs
		if info.StableID == "" {
			info.StableID = "serial:" + serial
		}
	}
	switch windows.GetDriveType(rootPtr) {
	case windows.DRIVE_REMOTE:
		info.IsNetwork = true
	case windows.DRIVE_REMOVABLE:
		info.IsRemovable = true
	}
	if sysRoot := os.Getenv("SystemRoot"); sysRoot != "" {
		if sysVol, err := volumePathName(sysRoot); err == nil && strings.EqualFold(sysVol, root) {
			info.IsSystem = true
		}
	}
	if info.StableID == "" {
		info.StableID = "path:" + strings.ToLower(root)
	}
	return info, nil
}

func volumePathName(path string) (string, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "", err
	}
	buf := make([]uint16, windows.MAX_PATH+1)
	if err := windows.GetVolumePathName(p, &buf[0], uint32(len(buf))); err != nil {
		return "", err
	}
	return windows.UTF16ToString(buf), nil
}

func volumeInformation(root string) (fsName, serial string, flags uint32, err error) {
	rootPtr, err := windows.UTF16PtrFromString(root)
	if err != nil {
		return "", "", 0, err
	}
	fsBuf := make([]uint16, windows.MAX_PATH+1)
	var serialNum, maxComp uint32
	if err := windows.GetVolumeInformation(rootPtr, nil, 0, &serialNum, &maxComp, &flags, &fsBuf[0], uint32(len(fsBuf))); err != nil {
		return "", "", 0, err
	}
	return windows.UTF16ToString(fsBuf), formatSerial(serialNum), flags, nil
}

func formatSerial(s uint32) string {
	return fmt.Sprintf("%04X-%04X", s>>16, s&0xFFFF)
}
