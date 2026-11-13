package volumes

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/jdolan-exalink/openvms-rec/internal/platform"
)

// State is the operational state of a volume. Only StateHealthy accepts writes.
type State string

const (
	StateHealthy  State = "healthy"
	StateFull     State = "full"     // at effective limit; cleanup pending
	StateDegraded State = "degraded" // IO error, read-only, probe failed
	StateMissing  State = "missing"  // path absent or a different disk is mounted there
	StateDisabled State = "disabled" // administratively off
	StateDraining State = "draining" // no new writes; segments kept/migrated
	StateUnready  State = "uninitialized"
)

// Warning codes surfaced to the UI; they never block on their own.
const (
	WarnSystemVolume    = "system_volume"
	WarnNetworkVolume   = "network_volume"
	WarnRemovableVolume = "removable_volume"
	WarnNoRedundancy    = "no_redundancy_assumed"
	WarnIdentityChanged = "identity_changed"
)

// MarkerFile is written at each volume root. It travels with the disk, so a
// drive-letter or mountpoint change still resolves to the same volume (ST-05),
// and an unmounted disk is never confused with the empty directory beneath it.
const MarkerFile = ".openvms-recorder-volume.json"

// Marker is the content of MarkerFile.
type Marker struct {
	VolumeID  string    `json:"volume_id"`
	StableID  string    `json:"stable_id"`
	CreatedAt time.Time `json:"created_at"`
	Format    int       `json:"format"`
}

// Prober abstracts the OS so inspection can be tested without real disks.
type Prober interface {
	Usage(path string) (platform.DiskUsage, error)
	Volume(path string) (platform.VolumeInfo, error)
}

// OSProber uses the real platform package.
type OSProber struct{}

func (OSProber) Usage(p string) (platform.DiskUsage, error)   { return platform.Usage(p) }
func (OSProber) Volume(p string) (platform.VolumeInfo, error) { return platform.Volume(p) }

// Spec is the desired configuration of one volume.
type Spec struct {
	ID       string
	Root     string
	Enabled  bool
	Draining bool
	Priority int
	Weight   int
	Policy   Policy
	Cameras  []string
}

// Status is the observed state of one volume after inspection.
type Status struct {
	Spec        Spec
	State       State
	Reason      string
	Info        platform.VolumeInfo
	Disk        platform.DiskUsage
	Usage       Usage
	Limits      Limits
	Warnings    []string
	InspectedAt time.Time
}

// Writable reports whether new segments may be placed on the volume.
func (s Status) Writable() bool { return s.State == StateHealthy }

// Inspector evaluates volume specs against the real filesystem.
type Inspector struct {
	Prober Prober
	// RecorderBytes returns this recorder's indexed bytes on a volume.
	RecorderBytes func(volumeID string) uint64
	Now           func() time.Time
}

// InspectAll inspects every spec and resolves duplicates: when two specs
// resolve to the same StableID the later one is marked degraded so capacity is
// never double counted.
func (in Inspector) InspectAll(specs []Spec) []Status {
	out := make([]Status, 0, len(specs))
	owner := map[string]string{}
	for _, sp := range specs {
		st := in.Inspect(sp)
		if st.Info.StableID != "" && st.State != StateMissing {
			if first, dup := owner[st.Info.StableID]; dup {
				st.State = StateDegraded
				st.Reason = fmt.Sprintf("same physical volume as %q (%s); capacity would be double counted", first, st.Info.StableID)
			} else {
				owner[st.Info.StableID] = sp.ID
			}
		}
		out = append(out, st)
	}
	return out
}

// Inspect evaluates one spec. It never creates, formats or repairs anything;
// see Initialize for the explicit first-use step.
func (in Inspector) Inspect(sp Spec) Status {
	now := time.Now
	if in.Now != nil {
		now = in.Now
	}
	st := Status{Spec: sp, InspectedAt: now().UTC()}

	fi, err := os.Stat(sp.Root)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return st.with(StateMissing, "root path does not exist")
	case err != nil:
		return st.with(StateDegraded, "stat root: "+err.Error())
	case !fi.IsDir():
		return st.with(StateDegraded, "root path is not a directory")
	}

	info, err := in.Prober.Volume(sp.Root)
	if err != nil {
		return st.with(StateDegraded, err.Error())
	}
	st.Info = info
	disk, err := in.Prober.Usage(sp.Root)
	if err != nil {
		return st.with(StateDegraded, err.Error())
	}
	st.Disk = disk
	if info.IsSystem {
		st.Warnings = append(st.Warnings, WarnSystemVolume)
	}
	if info.IsNetwork {
		st.Warnings = append(st.Warnings, WarnNetworkVolume)
	}
	if info.IsRemovable {
		st.Warnings = append(st.Warnings, WarnRemovableVolume)
	}
	st.Warnings = append(st.Warnings, WarnNoRedundancy)

	var used uint64
	if in.RecorderBytes != nil {
		used = in.RecorderBytes(sp.ID)
	}
	st.Usage = Usage{CapacityBytes: disk.TotalBytes, FreeBytes: disk.FreeBytes, RecorderBytes: used}
	st.Limits = Compute(sp.Policy, st.Usage)

	m, err := ReadMarker(sp.Root)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return st.with(StateUnready, "volume marker missing; validate the volume to initialize it (an unmounted disk looks like this too)")
	case err != nil:
		return st.with(StateDegraded, "read marker: "+err.Error())
	case m.VolumeID != sp.ID:
		return st.with(StateMissing, fmt.Sprintf("a different recorder volume (%q) is mounted at this path", m.VolumeID))
	case m.StableID != "" && info.StableID != "" && m.StableID != info.StableID:
		// Same disk content, new identity (cloned disk, restored backup, driver
		// change). Keep going but tell the operator.
		st.Warnings = append(st.Warnings, WarnIdentityChanged)
	}

	switch {
	case !sp.Enabled:
		return st.with(StateDisabled, "disabled by administrator")
	case disk.ReadOnly:
		return st.with(StateDegraded, "filesystem is read-only")
	case sp.Draining:
		return st.with(StateDraining, "draining: no new segments")
	case st.Limits.Full(st.Usage):
		return st.with(StateFull, fmt.Sprintf("at effective limit (%s)", st.Limits.Binding))
	}
	return st.with(StateHealthy, "")
}

func (s Status) with(state State, reason string) Status {
	s.State, s.Reason = state, reason
	return s
}

// ReadMarker reads the marker at root.
func ReadMarker(root string) (Marker, error) {
	b, err := os.ReadFile(filepath.Join(root, MarkerFile))
	if err != nil {
		return Marker{}, err
	}
	var m Marker
	if err := json.Unmarshal(b, &m); err != nil {
		return Marker{}, fmt.Errorf("corrupt marker: %w", err)
	}
	return m, nil
}

// ErrNotEmpty protects against initializing a directory that already holds data.
var ErrNotEmpty = errors.New("volume root is not empty and has no marker; refusing to initialize")

// Initialize writes the marker for a new volume. It is an explicit admin
// action (POST /storage/volumes/{id}/validate). It refuses non-empty roots
// without a marker so a stale mountpoint or someone else's data is never
// silently adopted, and it never formats anything.
func Initialize(p Prober, sp Spec, now time.Time) (Marker, error) {
	if m, err := ReadMarker(sp.Root); err == nil {
		if m.VolumeID != sp.ID {
			return Marker{}, fmt.Errorf("root already belongs to volume %q", m.VolumeID)
		}
		return m, nil
	}
	entries, err := os.ReadDir(sp.Root)
	if err != nil {
		return Marker{}, err
	}
	if len(entries) > 0 {
		return Marker{}, ErrNotEmpty
	}
	info, err := p.Volume(sp.Root)
	if err != nil {
		return Marker{}, err
	}
	m := Marker{VolumeID: sp.ID, StableID: info.StableID, CreatedAt: now.UTC(), Format: 1}
	b, _ := json.MarshalIndent(m, "", "  ")
	if err := writeFileAtomic(filepath.Join(sp.Root, MarkerFile), b); err != nil {
		return Marker{}, err
	}
	return m, nil
}

// writeFileAtomic writes to a temp file, fsyncs and renames over the target.
func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-marker-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
