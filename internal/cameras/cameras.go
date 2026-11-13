// Package cameras is the camera registry: cameras from the YAML file
// (read-only) plus cameras managed through the API, stored in SQLite.
// Credentials of API cameras live in a separate 0600 file in the data
// directory and are never returned by the API.
package cameras

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jdolan-exalink/openvms-rec/internal/config"
)

// Sources of a camera definition.
const (
	SourceConfig = "config" // recorder.yaml, edit the file and restart
	SourceAPI    = "api"    // created through the API/UI
)

// Errors returned by mutations.
var (
	ErrNotFound      = errors.New("camera not found")
	ErrExists        = errors.New("a camera with that id already exists")
	ErrReadOnly      = errors.New("camera is defined in the configuration file; edit recorder.yaml and restart")
	ErrHasRecordings = errors.New("camera has recordings; disable it instead of deleting it")
)

// InvalidError lists validation problems.
type InvalidError struct{ Issues []string }

func (e *InvalidError) Error() string { return "invalid camera: " + strings.Join(e.Issues, "; ") }

// Entry is a camera with its origin.
type Entry struct {
	config.Camera
	Source string
}

// Input creates or replaces an API camera. Username/Password nil keep the
// stored credentials; an empty Username removes them.
type Input struct {
	ID            string                  `json:"id"`
	Name          string                  `json:"name"`
	Enabled       *bool                   `json:"enabled"`
	MainURL       string                  `json:"main_url"`
	SubURL        string                  `json:"sub_url"`
	Transport     string                  `json:"rtsp_transport"`
	Audio         string                  `json:"audio"`
	Priority      *int                    `json:"priority"`
	DualRecording bool                    `json:"dual_recording"`
	Retention     *config.CameraRetention `json:"retention"`
	Username      *string                 `json:"username"`
	Password      *string                 `json:"password"`
	ONVIFURL      string                  `json:"onvif_url"`
	Events        string                  `json:"events"`
}

// Registry holds all cameras. Safe for concurrent use.
type Registry struct {
	db            *sql.DB
	static        []config.Camera
	staticSecrets config.Secrets
	credsFile     string
	globalDual    bool
	now           func() time.Time

	// OnChange is called after a camera is created, updated or deleted, with
	// the registry unlocked.
	OnChange func(id string)

	mu    sync.RWMutex
	api   map[string]config.Camera
	creds config.Secrets
}

// Options configures Open.
type Options struct {
	DB            *sql.DB // nil: only config cameras, mutations fail
	Static        []config.Camera
	StaticSecrets config.Secrets
	CredsFile     string // credentials of API cameras (0600)
	GlobalDual    bool
}

// Open loads API cameras from the database.
func Open(ctx context.Context, o Options) (*Registry, error) {
	r := &Registry{db: o.DB, static: o.Static, staticSecrets: o.StaticSecrets, credsFile: o.CredsFile,
		globalDual: o.GlobalDual, now: time.Now, api: map[string]config.Camera{}}
	if o.DB == nil {
		return r, nil
	}
	creds, err := config.LoadSecrets(o.CredsFile)
	if err != nil {
		return nil, err
	}
	r.creds = creds
	rows, err := o.DB.QueryContext(ctx, `SELECT id, name, enabled, stream_main_url, stream_sub_url, credentials_ref,
		rtsp_transport, audio, dual_recording, priority, retention_main_days, retention_sub_days, retention_min_days, onvif_url, events FROM cameras`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var c config.Camera
		var enabled bool
		var main, sub, min sql.NullInt64
		if err := rows.Scan(&c.ID, &c.Name, &enabled, &c.MainURL, &c.SubURL, &c.CredentialsRef, &c.Transport, &c.Audio,
			&c.DualRecording, &c.Priority, &main, &sub, &min, &c.ONVIFURL, &c.Events); err != nil {
			return nil, err
		}
		c.Enabled = &enabled
		if main.Valid || sub.Valid || min.Valid {
			c.Retention = &config.CameraRetention{MainDays: nullInt(main), SubDays: nullInt(sub), MinDays: nullInt(min)}
		}
		if r.isStatic(c.ID) {
			return nil, fmt.Errorf("camera %q is defined both in the configuration file and in the database; remove one", c.ID)
		}
		r.api[c.ID] = c
	}
	return r, rows.Err()
}

func nullInt(v sql.NullInt64) *int {
	if !v.Valid {
		return nil
	}
	i := int(v.Int64)
	return &i
}

func (r *Registry) isStatic(id string) bool {
	return slices.ContainsFunc(r.static, func(c config.Camera) bool { return c.ID == id })
}

// List returns config cameras first, then API cameras by id.
func (r *Registry) List() []Entry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Entry, 0, len(r.static)+len(r.api))
	for _, c := range r.static {
		out = append(out, Entry{c, SourceConfig})
	}
	ids := make([]string, 0, len(r.api))
	for id := range r.api {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		out = append(out, Entry{r.api[id], SourceAPI})
	}
	return out
}

// Get returns one camera.
func (r *Registry) Get(id string) (Entry, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, c := range r.static {
		if c.ID == id {
			return Entry{c, SourceConfig}, true
		}
	}
	c, ok := r.api[id]
	return Entry{c, SourceAPI}, ok
}

// StreamURL returns raw with the camera's credentials injected. The result
// is secret: never log it.
func (r *Registry) StreamURL(e Entry, raw string) (string, error) {
	if e.Source == SourceConfig {
		return r.staticSecrets.StreamURL(raw, e.CredentialsRef)
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.creds.StreamURL(raw, e.CredentialsRef)
}

// Credentials returns the stored credentials of a camera, if any.
func (r *Registry) Credentials(id string) (config.Credentials, bool) {
	e, ok := r.Get(id)
	if !ok || e.CredentialsRef == "" {
		return config.Credentials{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	s := r.creds
	if e.Source == SourceConfig {
		s = r.staticSecrets
	}
	c, ok := s.Cameras[e.CredentialsRef]
	return c, ok
}

func (r *Registry) build(id string, in Input) (config.Camera, error) {
	c := config.Camera{ID: id, Name: strings.TrimSpace(in.Name), Enabled: in.Enabled, MainURL: strings.TrimSpace(in.MainURL),
		SubURL: strings.TrimSpace(in.SubURL), Transport: in.Transport, Audio: in.Audio, Priority: 5,
		DualRecording: in.DualRecording, Retention: in.Retention, ONVIFURL: strings.TrimSpace(in.ONVIFURL), Events: in.Events}
	if in.Priority != nil {
		c.Priority = *in.Priority
	}
	if c.Enabled == nil {
		t := true
		c.Enabled = &t
	}
	if c.Transport == "" {
		c.Transport = "tcp"
	}
	if c.Audio == "" {
		c.Audio = "auto"
	}
	if c.Name == "" {
		c.Name = id
	}
	var issues []string
	if !config.ValidID(id) {
		issues = append(issues, "id: lowercase letters, digits and '-', up to 63 characters")
	}
	issues = append(issues, c.Validate(r.globalDual)...)
	if in.Username != nil && *in.Username != "" && strings.ContainsAny(*in.Username, ":@/") {
		issues = append(issues, "username: must not contain ':', '@' or '/'")
	}
	if len(issues) > 0 {
		return c, &InvalidError{issues}
	}
	return c, nil
}

// applyCreds updates r.creds for camera id per in and returns the new
// credentials_ref. Caller holds r.mu.
func (r *Registry) applyCreds(id string, in Input, prevRef string) (config.Secrets, string) {
	next := config.Secrets{Cameras: map[string]config.Credentials{}}
	for k, v := range r.creds.Cameras {
		next.Cameras[k] = v
	}
	ref := prevRef
	switch {
	case in.Username == nil && in.Password == nil:
	case in.Username != nil && *in.Username == "":
		delete(next.Cameras, id)
		ref = ""
	default:
		c := next.Cameras[id]
		if in.Username != nil {
			c.Username = *in.Username
		}
		if in.Password != nil {
			c.Password = *in.Password
		}
		next.Cameras[id] = c
		ref = id
	}
	return next, ref
}

func (r *Registry) save(ctx context.Context, c config.Camera, create bool) error {
	var main, sub, min any
	if rt := c.Retention; rt != nil {
		if rt.MainDays != nil {
			main = *rt.MainDays
		}
		if rt.SubDays != nil {
			sub = *rt.SubDays
		}
		if rt.MinDays != nil {
			min = *rt.MinDays
		}
	}
	now := r.now().UnixMilli()
	if create {
		_, err := r.db.ExecContext(ctx, `INSERT INTO cameras (id, name, enabled, stream_main_url, stream_sub_url, credentials_ref,
			rtsp_transport, audio, dual_recording, priority, retention_main_days, retention_sub_days, retention_min_days, onvif_url, events, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			c.ID, c.Name, c.IsEnabled(), c.MainURL, c.SubURL, c.CredentialsRef, c.Transport, c.Audio, c.DualRecording, c.Priority, main, sub, min,
			c.ONVIFURL, c.Events, now, now)
		return err
	}
	_, err := r.db.ExecContext(ctx, `UPDATE cameras SET name=?, enabled=?, stream_main_url=?, stream_sub_url=?, credentials_ref=?,
		rtsp_transport=?, audio=?, dual_recording=?, priority=?, retention_main_days=?, retention_sub_days=?, retention_min_days=?,
		onvif_url=?, events=?, updated_at=?
		WHERE id=?`, c.Name, c.IsEnabled(), c.MainURL, c.SubURL, c.CredentialsRef, c.Transport, c.Audio, c.DualRecording, c.Priority,
		main, sub, min, c.ONVIFURL, c.Events, now, c.ID)
	return err
}

// Create adds an API camera.
func (r *Registry) Create(ctx context.Context, in Input) (Entry, error) {
	if r.db == nil {
		return Entry{}, ErrReadOnly
	}
	c, err := r.build(in.ID, in)
	if err != nil {
		return Entry{}, err
	}
	r.mu.Lock()
	if _, ok := r.api[c.ID]; ok || r.isStatic(c.ID) {
		r.mu.Unlock()
		return Entry{}, ErrExists
	}
	creds, ref := r.applyCreds(c.ID, in, "")
	c.CredentialsRef = ref
	err = r.commit(ctx, c, creds, true)
	r.mu.Unlock()
	if err != nil {
		return Entry{}, err
	}
	r.changed(c.ID)
	return Entry{c, SourceAPI}, nil
}

// Update replaces an API camera.
func (r *Registry) Update(ctx context.Context, id string, in Input) (Entry, error) {
	if r.isStatic(id) {
		return Entry{}, ErrReadOnly
	}
	c, err := r.build(id, in)
	if err != nil {
		return Entry{}, err
	}
	r.mu.Lock()
	prev, ok := r.api[id]
	if !ok {
		r.mu.Unlock()
		return Entry{}, ErrNotFound
	}
	creds, ref := r.applyCreds(id, in, prev.CredentialsRef)
	c.CredentialsRef = ref
	err = r.commit(ctx, c, creds, false)
	r.mu.Unlock()
	if err != nil {
		return Entry{}, err
	}
	r.changed(id)
	return Entry{c, SourceAPI}, nil
}

// commit writes credentials then the row. Caller holds r.mu.
func (r *Registry) commit(ctx context.Context, c config.Camera, creds config.Secrets, create bool) error {
	if !sameCreds(r.creds, creds) {
		if err := config.SaveSecrets(r.credsFile, creds); err != nil {
			return fmt.Errorf("save credentials: %w", err)
		}
	}
	if err := r.save(ctx, c, create); err != nil {
		// Keep the file consistent with the database.
		if !sameCreds(r.creds, creds) {
			config.SaveSecrets(r.credsFile, r.creds)
		}
		return err
	}
	r.creds = creds
	r.api[c.ID] = c
	return nil
}

func sameCreds(a, b config.Secrets) bool {
	if len(a.Cameras) != len(b.Cameras) {
		return false
	}
	for k, v := range a.Cameras {
		if w, ok := b.Cameras[k]; !ok || w != v {
			return false
		}
	}
	return true
}

// Delete removes an API camera that has no recordings.
func (r *Registry) Delete(ctx context.Context, id string) error {
	if r.isStatic(id) {
		return ErrReadOnly
	}
	r.mu.Lock()
	if _, ok := r.api[id]; !ok {
		r.mu.Unlock()
		return ErrNotFound
	}
	var n int
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM segments WHERE camera_id = ? AND status NOT IN ('deleted')`, id).Scan(&n); err != nil {
		r.mu.Unlock()
		return err
	}
	if n > 0 {
		r.mu.Unlock()
		return ErrHasRecordings
	}
	if _, err := r.db.ExecContext(ctx, `DELETE FROM cameras WHERE id = ?`, id); err != nil {
		r.mu.Unlock()
		return err
	}
	delete(r.api, id)
	if _, ok := r.creds.Cameras[id]; ok {
		creds, _ := r.applyCreds(id, Input{Username: new(string)}, "")
		if err := config.SaveSecrets(r.credsFile, creds); err == nil {
			r.creds = creds
		}
	}
	r.mu.Unlock()
	r.changed(id)
	return nil
}

func (r *Registry) changed(id string) {
	if r.OnChange != nil {
		r.OnChange(id)
	}
}
