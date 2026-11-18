// Package config loads and validates the Recorder YAML configuration.
//
// The authoritative description of the file is api/config.schema.json; this
// package mirrors it and adds semantic checks (threshold ordering, duplicate
// volume IDs, ...) that JSON Schema cannot express.
package config

import (
	"errors"
	"fmt"
	"io"
	"net/netip"
	"net/url"
	"os"
	"regexp"
	"runtime"
	"strings"

	"gopkg.in/yaml.v3"
)

// Recording modes. Only continuous_with_event_markers is in the MVP (ADR-001).
const (
	ModeContinuousWithEventMarkers = "continuous_with_event_markers"
)

// Placement policies (ADR-002).
const (
	PlacementBalanced       = "balanced"
	PlacementCameraAffinity = "camera_affinity"
	PlacementManual         = "manual"
)

// Pressure policies: what to do when retention minimums and the free-space
// reserve cannot both be honoured.
const (
	// PressureOverwriteOldest deletes the oldest unprotected segments even if
	// that cuts into a camera's minimum retention, and raises an alert.
	PressureOverwriteOldest = "overwrite_oldest_unprotected"
	// PressureProtectMinimum never deletes below minimum retention; the volume
	// is reported full and stops accepting new segments instead.
	PressureProtectMinimum = "protect_minimum_retention"
)

// Config is the root of the YAML file.
type Config struct {
	Server    Server       `yaml:"server"`
	Data      Data         `yaml:"data"`
	Recording Recording    `yaml:"recording"`
	Storage   Storage      `yaml:"storage"`
	Retention Retention    `yaml:"retention"`
	Recovery  Recovery     `yaml:"recovery"`
	Events    EventsConfig `yaml:"events"`
	Core      CoreConfig   `yaml:"core"`
	Cameras   []Camera     `yaml:"cameras"`
}

// CoreConfig configures the optional Phase 4 integration adapter with OpenVMS Core.
// Recorder stays fully autonomous when Enabled is false or Core is unreachable.
type CoreConfig struct {
	Enabled               bool   `yaml:"enabled"`
	CoreURL               string `yaml:"core_url"`
	NodeID                string `yaml:"node_id"`
	SiteID                string `yaml:"site_id"`
	SyncIntervalSeconds   int    `yaml:"sync_interval_seconds"`
	PushEvents            *bool  `yaml:"push_events"`
	PushHealth            *bool  `yaml:"push_health"`
	TLSInsecureSkipVerify bool   `yaml:"tls_insecure_skip_verify"`
}

// EventsConfig tunes event normalization, TVT active HTTP polling, and optional
// local NVR video classification.
type EventsConfig struct {
	MinConfidence  float64          `yaml:"min_confidence"`
	TVTPollSeconds int              `yaml:"tvt_poll_seconds"`
	Classifier     ClassifierConfig `yaml:"classifier"`
}

// ClassifierConfig selects an optional local AI inference adapter triggered on
// motion frames ("none", "http", or "command").
type ClassifierConfig struct {
	Mode          string   `yaml:"mode"`
	Endpoint      string   `yaml:"endpoint"`
	Command       []string `yaml:"command"`
	MinConfidence float64  `yaml:"min_confidence"`
	TimeoutMS     int      `yaml:"timeout_ms"`
}

// Camera is a recorded camera. URLs must not contain credentials: they live
// in the secrets file under credentials_ref.
type Camera struct {
	ID             string           `yaml:"id"`
	Name           string           `yaml:"name"`
	Enabled        *bool            `yaml:"enabled"`
	Host           string           `yaml:"host"`
	ONVIFPort      int              `yaml:"onvif_port"`
	ONVIFEvents    *bool            `yaml:"onvif_events"`
	Brand          string           `yaml:"brand"`
	NVRMotion      bool             `yaml:"nvr_motion"`
	MainURL        string           `yaml:"main_url"`
	SubURL         string           `yaml:"sub_url"`
	CredentialsRef string           `yaml:"credentials_ref"`
	Transport      string           `yaml:"transport"`
	Audio          string           `yaml:"audio"`
	Priority       int              `yaml:"priority"`
	DualRecording  bool             `yaml:"dual_recording"`
	Retention      *CameraRetention `yaml:"retention"`
}

// EffectiveHost returns Host if set, or the hostname parsed from MainURL.
func (c Camera) EffectiveHost() string {
	if h := strings.TrimSpace(c.Host); h != "" {
		return h
	}
	u, err := url.Parse(c.MainURL)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// EffectiveONVIFPort returns ONVIFPort if > 0, or 80.
func (c Camera) EffectiveONVIFPort() int {
	if c.ONVIFPort > 0 && c.ONVIFPort <= 65535 {
		return c.ONVIFPort
	}
	return 80
}

// PollONVIFEvents defaults to true.
func (c Camera) PollONVIFEvents() bool { return c.ONVIFEvents == nil || *c.ONVIFEvents }

// CameraRetention overrides global retention days for one camera.
type CameraRetention struct {
	MainDays *int `yaml:"main_days"`
	SubDays  *int `yaml:"sub_days"`
	MinDays  *int `yaml:"min_days"`
}

// Validate checks one camera's fields (not its id). globalDual is
// recording.dual_recording. Issues are "field: problem".
func (cam Camera) Validate(globalDual bool) []string {
	var out []string
	if cam.MainURL == "" {
		out = append(out, "main_url: required")
	}
	for _, f := range []struct{ name, raw string }{{"main_url", cam.MainURL}, {"sub_url", cam.SubURL}} {
		if f.raw == "" {
			continue
		}
		u, err := url.Parse(f.raw)
		switch {
		case err != nil || u.Host == "":
			out = append(out, f.name+": invalid url")
		case u.Scheme != "rtsp" && u.Scheme != "rtsps":
			out = append(out, f.name+": scheme must be rtsp or rtsps")
		case u.User != nil:
			out = append(out, f.name+": must not contain credentials; use credentials_ref and the secrets file")
		}
	}
	if cam.Transport != "" && cam.Transport != "tcp" && cam.Transport != "udp" {
		out = append(out, "transport: must be tcp or udp")
	}
	switch cam.Audio {
	case "", "auto", "copy", "aac", "none":
	default:
		out = append(out, "audio: must be auto, copy, aac or none")
	}
	if (cam.DualRecording || globalDual) && cam.SubURL == "" {
		out = append(out, "sub_url: required for dual_recording")
	}
	if cam.Priority < 0 || cam.Priority > 10 {
		out = append(out, "priority: must be in [0, 10]")
	}
	if r := cam.Retention; r != nil {
		for _, d := range []*int{r.MainDays, r.SubDays, r.MinDays} {
			if d != nil && *d < 0 {
				out = append(out, "retention: day counts must be >= 0")
				break
			}
		}
	}
	return out
}

// ValidID reports whether id is a valid camera or volume id.
func ValidID(id string) bool { return volumeID.MatchString(id) }

// IsEnabled defaults to true.
func (c Camera) IsEnabled() bool { return c.Enabled == nil || *c.Enabled }

type Server struct {
	Listen string `yaml:"listen"`
	// APIKeysFile holds "name:sha256" lines (see `openvms-recorder apikey create`).
	// Required when listen is not a loopback address.
	APIKeysFile string `yaml:"api_keys_file"`
	// CameraNetworks restricts ONVIF probe and RTSP test targets to these
	// CIDR ranges (SSRF protection). Empty allows any reachable host.
	CameraNetworks []string `yaml:"camera_networks"`
	// TLSCert and TLSKey enable HTTPS when both are set.
	TLSCert string `yaml:"tls_cert"`
	TLSKey  string `yaml:"tls_key"`
}

// Data holds paths for state that must live outside the video volumes.
type Data struct {
	// Dir holds the SQLite database, logs and diagnostics.
	Dir string `yaml:"dir"`
	// SecretsFile holds camera credentials; must be mode 0600 on Unix.
	SecretsFile string `yaml:"secrets_file"`
}

type Recording struct {
	Mode           string `yaml:"mode"`
	DefaultStream  string `yaml:"default_stream"`
	DualRecording  bool   `yaml:"dual_recording"`
	SegmentSeconds int    `yaml:"segment_seconds"`
	FFmpegPath     string `yaml:"ffmpeg_path"`
}

type Storage struct {
	DefaultMaxPercent    float64  `yaml:"default_max_percent"`
	CleanupStartPercent  float64  `yaml:"cleanup_start_percent"`
	CleanupTargetPercent float64  `yaml:"cleanup_target_percent"`
	MinFreeGB            float64  `yaml:"min_free_gb"`
	Placement            string   `yaml:"placement"`
	PressurePolicy       string   `yaml:"pressure_policy"`
	AllowSystemVolume    bool     `yaml:"allow_system_volume"`
	AllowNetworkVolumes  bool     `yaml:"allow_network_volumes"`
	Volumes              []Volume `yaml:"volumes"`
}

// Volume is a recording root. Exactly one of the path fields must resolve on
// the running OS: path_windows / path_linux win over the generic path.
type Volume struct {
	ID            string   `yaml:"id"`
	Path          string   `yaml:"path"`
	PathWindows   string   `yaml:"path_windows"`
	PathLinux     string   `yaml:"path_linux"`
	Enabled       *bool    `yaml:"enabled"`
	MaxPercent    float64  `yaml:"max_percent"`
	QuotaGB       float64  `yaml:"quota_gb"`
	FolderQuotaGB float64  `yaml:"folder_quota_gb"`
	MinFreeGB     *float64 `yaml:"min_free_gb"`
	Priority      int      `yaml:"priority"`
	Weight        int      `yaml:"weight"`
	Cameras       []string `yaml:"cameras"`
}

type Retention struct {
	MainDays                int  `yaml:"main_days"`
	LowDays                 int  `yaml:"low_days"`
	MinDays                 int  `yaml:"min_days"`
	PreserveProtectedEvents bool `yaml:"preserve_protected_events"`
}

type Recovery struct {
	CameraSDBackfill string  `yaml:"camera_sd_backfill"`
	MaxBandwidthMbps float64 `yaml:"max_bandwidth_mbps"`
	AutoSyncSeconds  int     `yaml:"auto_sync_seconds"`
}

var volumeID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// Default returns a configuration with the PRD's initial values.
func Default() Config {
	return Config{
		Server: Server{Listen: "127.0.0.1:8640"},
		Data:   Data{Dir: defaultDataDir()},
		Recording: Recording{
			Mode:           ModeContinuousWithEventMarkers,
			DefaultStream:  "main",
			SegmentSeconds: 60,
			FFmpegPath:     "ffmpeg",
		},
		Storage: Storage{
			DefaultMaxPercent:    90,
			CleanupStartPercent:  87,
			CleanupTargetPercent: 84,
			MinFreeGB:            100,
			Placement:            PlacementBalanced,
			PressurePolicy:       PressureOverwriteOldest,
		},
		Retention: Retention{
			MainDays:                7,
			LowDays:                 30,
			MinDays:                 1,
			PreserveProtectedEvents: true,
		},
		Recovery: Recovery{CameraSDBackfill: "when_supported", MaxBandwidthMbps: 20, AutoSyncSeconds: 300},
		Events: EventsConfig{
			TVTPollSeconds: 3,
			Classifier:     ClassifierConfig{Mode: "none", MinConfidence: 0.5, TimeoutMS: 2500},
		},
		Core: CoreConfig{
			SyncIntervalSeconds: 30,
		},
	}
}

func defaultDataDir() string {
	if runtime.GOOS == "windows" {
		if pd := os.Getenv("ProgramData"); pd != "" {
			return pd + `\OpenVMS\Recorder`
		}
		return `C:\ProgramData\OpenVMS\Recorder`
	}
	return "/var/lib/openvms-recorder"
}

// Load reads a YAML file on top of Default(), applies environment overrides
// and validates the result. Unknown keys are rejected to catch typos.
func Load(path string) (Config, error) {
	cfg := Default()
	if path != "" {
		f, err := os.Open(path)
		if err != nil {
			return Config{}, fmt.Errorf("open config: %w", err)
		}
		defer f.Close()
		dec := yaml.NewDecoder(f)
		dec.KnownFields(true)
		if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
			return Config{}, fmt.Errorf("parse config %s: %w", path, err)
		}
	}
	ApplyEnv(&cfg, os.LookupEnv)
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// ApplyEnv overrides a few deployment-specific values from the environment.
// Secrets are never read from here; use data.secrets_file.
func ApplyEnv(cfg *Config, lookup func(string) (string, bool)) {
	if v, ok := lookup("OPENVMS_RECORDER_LISTEN"); ok && v != "" {
		cfg.Server.Listen = v
	}
	if v, ok := lookup("OPENVMS_RECORDER_DATA_DIR"); ok && v != "" {
		cfg.Data.Dir = v
	}
	if v, ok := lookup("OPENVMS_RECORDER_SECRETS_FILE"); ok && v != "" {
		cfg.Data.SecretsFile = v
	}
}

// ResolvedPath returns the volume root for the given GOOS.
func (v Volume) ResolvedPath(goos string) string {
	switch goos {
	case "windows":
		if v.PathWindows != "" {
			return v.PathWindows
		}
	case "linux":
		if v.PathLinux != "" {
			return v.PathLinux
		}
	}
	return v.Path
}

// IsEnabled defaults to true when unset.
func (v Volume) IsEnabled() bool { return v.Enabled == nil || *v.Enabled }

// EffectiveMaxPercent falls back to the storage default.
func (v Volume) EffectiveMaxPercent(s Storage) float64 {
	if v.MaxPercent > 0 {
		return v.MaxPercent
	}
	return s.DefaultMaxPercent
}

// EffectiveMinFreeGB falls back to the storage default.
func (v Volume) EffectiveMinFreeGB(s Storage) float64 {
	if v.MinFreeGB != nil {
		return *v.MinFreeGB
	}
	return s.MinFreeGB
}

// EffectiveWeight defaults to 1.
func (v Volume) EffectiveWeight() int {
	if v.Weight > 0 {
		return v.Weight
	}
	return 1
}

// Validate checks semantic rules. All problems are reported together.
func (c Config) Validate() error {
	var errs []string
	add := func(format string, a ...any) { errs = append(errs, fmt.Sprintf(format, a...)) }

	if (c.Server.TLSCert == "") != (c.Server.TLSKey == "") {
		add("server: tls_cert and tls_key must both be set")
	}
	for _, cidr := range c.Server.CameraNetworks {
		if _, err := netip.ParsePrefix(strings.TrimSpace(cidr)); err != nil {
			add("server.camera_networks: invalid CIDR %q: %v", cidr, err)
		}
	}

	if c.Recording.Mode != ModeContinuousWithEventMarkers {
		add("recording.mode: %q not supported (only %q in this version)", c.Recording.Mode, ModeContinuousWithEventMarkers)
	}
	if c.Recording.DefaultStream != "main" && c.Recording.DefaultStream != "sub" {
		add("recording.default_stream: must be main or sub")
	}
	s := c.Storage
	errs = append(errs, ValidateStoragePolicy(s)...)
	seen := map[string]bool{}
	for i, v := range s.Volumes {
		where := fmt.Sprintf("storage.volumes[%d]", i)
		if v.ID == "" {
			add("%s.id: required", where)
		} else if !volumeID.MatchString(v.ID) {
			add("%s.id: %q must match %s", where, v.ID, volumeID)
		} else if seen[v.ID] {
			add("%s.id: duplicate %q", where, v.ID)
		}
		seen[v.ID] = true
		if v.Path == "" && v.PathWindows == "" && v.PathLinux == "" {
			add("%s: one of path, path_windows, path_linux is required", where)
		}
		if v.MaxPercent < 0 || v.MaxPercent > 100 {
			add("%s.max_percent: %.2f must be in (0, 100]", where, v.MaxPercent)
		}
		if v.QuotaGB < 0 {
			add("%s.quota_gb: must be >= 0", where)
		}
		if v.FolderQuotaGB < 0 {
			add("%s.folder_quota_gb: must be >= 0", where)
		}
		if v.MinFreeGB != nil && *v.MinFreeGB < 0 {
			add("%s.min_free_gb: must be >= 0", where)
		}
		if v.Weight < 0 {
			add("%s.weight: must be >= 0", where)
		}
	}
	if s.Placement == PlacementManual {
		assigned := false
		for _, v := range s.Volumes {
			if len(v.Cameras) > 0 {
				assigned = true
			}
		}
		if len(s.Volumes) > 0 && !assigned {
			add("storage.placement manual: at least one volume needs a cameras list")
		}
	}
	r := c.Retention
	if r.MainDays < 0 || r.LowDays < 0 || r.MinDays < 0 {
		add("retention: day counts must be >= 0")
	}
	if r.MainDays > 0 && r.MinDays > r.MainDays {
		add("retention.min_days (%d) must not exceed main_days (%d)", r.MinDays, r.MainDays)
	}
	switch c.Recovery.CameraSDBackfill {
	case "when_supported", "recover_all", "recover_events_only", "index_remote_only", "disabled":
	default:
		add("recovery.camera_sd_backfill: %q not recognised", c.Recovery.CameraSDBackfill)
	}
	if c.Recovery.AutoSyncSeconds < 0 || c.Recovery.AutoSyncSeconds > 86400 {
		add("recovery.auto_sync_seconds: %d must be in [0, 86400]", c.Recovery.AutoSyncSeconds)
	}
	if c.Events.MinConfidence < 0 || c.Events.MinConfidence > 1 {
		add("events.min_confidence: %.2f must be in [0, 1]", c.Events.MinConfidence)
	}
	if c.Events.TVTPollSeconds < 0 || c.Events.TVTPollSeconds > 300 {
		add("events.tvt_poll_seconds: %d must be in [0, 300]", c.Events.TVTPollSeconds)
	}
	switch c.Events.Classifier.Mode {
	case "", "none":
	case "http":
		if u, err := url.Parse(strings.TrimSpace(c.Events.Classifier.Endpoint)); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			add("events.classifier.endpoint: valid http/https URL required when mode=http")
		}
	case "command":
		if len(c.Events.Classifier.Command) == 0 || strings.TrimSpace(c.Events.Classifier.Command[0]) == "" {
			add("events.classifier.command: non-empty command required when mode=command")
		}
	default:
		add("events.classifier.mode: %q must be none, http or command", c.Events.Classifier.Mode)
	}
	if c.Events.Classifier.MinConfidence < 0 || c.Events.Classifier.MinConfidence > 1 {
		add("events.classifier.min_confidence: %.2f must be in [0, 1]", c.Events.Classifier.MinConfidence)
	}
	if c.Recording.SegmentSeconds < 2 || c.Recording.SegmentSeconds > 600 {
		add("recording.segment_seconds: %d must be in [2, 600]", c.Recording.SegmentSeconds)
	}
	camSeen := map[string]bool{}
	for i, cam := range c.Cameras {
		where := fmt.Sprintf("cameras[%d]", i)
		if !volumeID.MatchString(cam.ID) {
			add("%s.id: %q must match %s", where, cam.ID, volumeID)
		} else if camSeen[cam.ID] {
			add("%s.id: duplicate %q", where, cam.ID)
		}
		camSeen[cam.ID] = true
		for _, issue := range cam.Validate(c.Recording.DualRecording) {
			add("%s.%s", where, issue)
		}
	}
	if c.Core.Enabled {
		u, err := url.Parse(strings.TrimSpace(c.Core.CoreURL))
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			add("core.core_url: valid http/https URL required when core.enabled=true")
		}
	}
	if c.Core.SyncIntervalSeconds < 0 || c.Core.SyncIntervalSeconds > 3600 {
		add("core.sync_interval_seconds: %d must be in [0, 3600]", c.Core.SyncIntervalSeconds)
	}
	if c.Data.Dir == "" {
		add("data.dir: required")
	}
	if len(errs) > 0 {
		return fmt.Errorf("invalid configuration:\n  - %s", strings.Join(errs, "\n  - "))
	}
	return nil
}

// ValidateStoragePolicy validates global storage thresholds, reserve, placement and pressure policy.
func ValidateStoragePolicy(s Storage) []string {
	var errs []string
	add := func(format string, a ...any) { errs = append(errs, fmt.Sprintf(format, a...)) }
	pct := func(name string, v float64) {
		if v <= 0 || v > 100 {
			add("storage.%s: %.2f must be in (0, 100]", name, v)
		}
	}
	pct("default_max_percent", s.DefaultMaxPercent)
	pct("cleanup_start_percent", s.CleanupStartPercent)
	pct("cleanup_target_percent", s.CleanupTargetPercent)
	if s.CleanupTargetPercent >= s.CleanupStartPercent {
		add("storage: cleanup_target_percent (%.2f) must be below cleanup_start_percent (%.2f)", s.CleanupTargetPercent, s.CleanupStartPercent)
	}
	if s.CleanupStartPercent >= s.DefaultMaxPercent {
		add("storage: cleanup_start_percent (%.2f) must be below default_max_percent (%.2f)", s.CleanupStartPercent, s.DefaultMaxPercent)
	}
	if s.MinFreeGB < 0 {
		add("storage.min_free_gb: must be >= 0")
	}
	switch s.Placement {
	case PlacementBalanced, PlacementCameraAffinity, PlacementManual:
	default:
		add("storage.placement: %q must be balanced, camera_affinity or manual", s.Placement)
	}
	switch s.PressurePolicy {
	case PressureOverwriteOldest, PressureProtectMinimum:
	default:
		add("storage.pressure_policy: %q must be %s or %s", s.PressurePolicy, PressureOverwriteOldest, PressureProtectMinimum)
	}
	return errs
}
