//go:build linux

package platform

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// Filesystem magic numbers from statfs(2).
var fsMagic = map[int64]string{
	0xEF53:     "ext4", // ext2/3/4 share the magic
	0x58465342: "xfs",
	0x9123683E: "btrfs",
	0x2FC12FC1: "zfs",
	0x6969:     "nfs",
	0xFF534D42: "cifs",
	0xFE534D42: "smb2",
	0x01021994: "tmpfs",
	0x65735546: "fuse",
	0x794C7630: "overlayfs",
	0x4D44:     "vfat",
	0x5346544E: "ntfs",
	0x2011BAB0: "exfat",
	0xF2F52010: "f2fs",
}

var networkFS = map[string]bool{"nfs": true, "cifs": true, "smb2": true, "fuse": true}

func usage(path string) (DiskUsage, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return DiskUsage{}, err
	}
	bs := uint64(st.Bsize)
	return DiskUsage{
		TotalBytes: st.Blocks * bs,
		FreeBytes:  st.Bavail * bs,
		ReadOnly:   st.Flags&unix.ST_RDONLY != 0,
	}, nil
}

func volume(path string) (VolumeInfo, error) {
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		return VolumeInfo{}, err
	}
	var sfs unix.Statfs_t
	if err := unix.Statfs(path, &sfs); err != nil {
		return VolumeInfo{}, err
	}
	devMajor, devMinor := unix.Major(uint64(st.Dev)), unix.Minor(uint64(st.Dev))
	info := VolumeInfo{
		StableID: fmt.Sprintf("dev:%d:%d", devMajor, devMinor),
		FSType:   fsMagic[int64(sfs.Type)],
	}
	if info.FSType == "" {
		info.FSType = fmt.Sprintf("0x%x", sfs.Type)
	}
	info.IsNetwork = networkFS[info.FSType]

	if mp, src, ok := findMount(devMajor, devMinor, path); ok {
		info.MountPoint = mp
		info.Device = src
		if uuid := uuidForDevice(src); uuid != "" {
			info.StableID = "uuid:" + uuid
		}
		info.IsRemovable = isRemovable(src)
	}
	// The system volume is the one holding "/".
	var root unix.Stat_t
	if err := unix.Stat("/", &root); err == nil && root.Dev == st.Dev {
		info.IsSystem = true
	}
	return info, nil
}

// findMount scans /proc/self/mountinfo for the mount with the given device
// number that is the longest prefix of path.
func findMount(major, minor uint32, path string) (mountPoint, source string, ok bool) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return "", "", false
	}
	defer f.Close()
	abs, _ := filepath.Abs(path)
	want := fmt.Sprintf("%d:%d", major, minor)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		// 36 35 98:0 /mnt1 /mnt/parent rw,noatime master:1 - ext3 /dev/root rw,errors=continue
		fields := strings.Fields(sc.Text())
		if len(fields) < 10 || fields[2] != want {
			continue
		}
		mp := unescapeMount(fields[4])
		if !strings.HasPrefix(abs, mp) {
			continue
		}
		sep := -1
		for i, f := range fields {
			if f == "-" {
				sep = i
				break
			}
		}
		src := ""
		if sep >= 0 && sep+2 < len(fields) {
			src = fields[sep+2]
		}
		if len(mp) > len(mountPoint) {
			mountPoint, source, ok = mp, src, true
		}
	}
	return mountPoint, source, ok
}

func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// uuidForDevice resolves a block device to its filesystem UUID through
// /dev/disk/by-uuid. Returns "" when not resolvable (containers, ZFS, tmpfs).
func uuidForDevice(dev string) string {
	if !strings.HasPrefix(dev, "/dev/") {
		return ""
	}
	target, err := filepath.EvalSymlinks(dev)
	if err != nil {
		return ""
	}
	entries, err := os.ReadDir("/dev/disk/by-uuid")
	if err != nil {
		return ""
	}
	for _, e := range entries {
		p, err := filepath.EvalSymlinks(filepath.Join("/dev/disk/by-uuid", e.Name()))
		if err == nil && p == target {
			return e.Name()
		}
	}
	return ""
}

// isRemovable reads /sys/block/<disk>/removable for the device's parent disk.
func isRemovable(dev string) bool {
	target, err := filepath.EvalSymlinks(dev)
	if err != nil {
		return false
	}
	name := filepath.Base(target)
	sysPath, err := filepath.EvalSymlinks(filepath.Join("/sys/class/block", name))
	if err != nil {
		return false
	}
	// Partitions live under their disk; walk up until a "removable" file exists.
	for p := sysPath; p != "/" && p != "."; p = filepath.Dir(p) {
		if b, err := os.ReadFile(filepath.Join(p, "removable")); err == nil {
			return strings.TrimSpace(string(b)) == "1"
		}
	}
	return false
}
