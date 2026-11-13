// Package events collects camera events (ONVIF PullPoint) into the events
// table. Events are markers on the continuous recording: they never start or
// stop recording.
package events

import (
	"context"
	"crypto/sha1" // #nosec G505 -- dedupe key only, not security.
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/jdolan-exalink/openvms-rec/internal/onvif"
)

// Source is the events.source value of ONVIF events.
const Source = "onvif_events"

// Target is one camera to collect from. Credentials stay in memory.
type Target struct {
	CameraID  string
	DeviceURL string
	Username  string
	Password  string
}

// Status is the live state of a camera's collection.
type Status struct {
	State     string    `json:"state"` // connecting, subscribed, backoff, stopped
	LastEvent time.Time `json:"last_event,omitempty"`
	LastError string    `json:"last_error,omitempty"`
	Events    int64     `json:"events"`
}

// Collector runs one PullPoint loop per camera.
type Collector struct {
	DB        *sql.DB
	Log       *slog.Logger
	Transport http.RoundTripper // nil: onvif.ProbeTransport()
	Now       func() time.Time

	// Tunables (zero: defaults). Tests shorten them.
	PullTimeout time.Duration // PullMessages long-poll, default 10 s
	Resubscribe time.Duration // new subscription after, default 4 min
	BackoffMax  time.Duration // default 60 s
	backoffMin  time.Duration

	mu      sync.Mutex
	running map[string]*job
}

type job struct {
	target Target
	cancel context.CancelFunc
	done   chan struct{}
	status Status
}

func (c *Collector) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// Apply starts, restarts or (with t nil) stops collection for cameraID.
func (c *Collector) Apply(cameraID string, t *Target) {
	c.mu.Lock()
	if c.running == nil {
		c.running = map[string]*job{}
	}
	old := c.running[cameraID]
	if old != nil && t != nil && old.target == *t {
		c.mu.Unlock()
		return
	}
	delete(c.running, cameraID)
	var j *job
	if t != nil {
		ctx, cancel := context.WithCancel(context.Background())
		j = &job{target: *t, cancel: cancel, done: make(chan struct{}), status: Status{State: "connecting"}}
		c.running[cameraID] = j
		go func() {
			defer close(j.done)
			c.loop(ctx, j)
		}()
	}
	c.mu.Unlock()
	if old != nil {
		old.cancel()
		<-old.done
	}
}

// StopAll stops every loop and waits for them.
func (c *Collector) StopAll() {
	c.mu.Lock()
	jobs := c.running
	c.running = nil
	c.mu.Unlock()
	for _, j := range jobs {
		j.cancel()
	}
	for _, j := range jobs {
		<-j.done
	}
}

// Status returns the state of cameraID, ok false when it is not collected.
func (c *Collector) Status(cameraID string) (Status, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	j, ok := c.running[cameraID]
	if !ok {
		return Status{}, false
	}
	return j.status, true
}

func (c *Collector) set(j *job, f func(*Status)) {
	c.mu.Lock()
	f(&j.status)
	c.mu.Unlock()
}

func (c *Collector) loop(ctx context.Context, j *job) {
	backoff := 5 * time.Second
	if c.backoffMin > 0 {
		backoff = c.backoffMin
	}
	maxB := c.BackoffMax
	if maxB <= 0 {
		maxB = 60 * time.Second
	}
	for ctx.Err() == nil {
		subscribed, err := c.session(ctx, j)
		if ctx.Err() != nil {
			break
		}
		if subscribed {
			backoff = 5 * time.Second
			if c.backoffMin > 0 {
				backoff = c.backoffMin
			}
		}
		msg := "subscription ended"
		if err != nil {
			msg = redactErr(err)
		}
		c.set(j, func(s *Status) { s.State, s.LastError = "backoff", msg })
		c.Log.Warn("onvif events", "camera", j.target.CameraID, "err", msg, "retry_in", backoff)
		select {
		case <-ctx.Done():
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxB)
	}
	c.set(j, func(s *Status) { s.State = "stopped" })
}

func redactErr(err error) string {
	var ce *onvif.CallError
	if errors.As(err, &ce) {
		return "onvif " + ce.Code
	}
	if errors.Is(err, onvif.ErrProbeAuth) {
		return "auth_failed"
	}
	s := err.Error()
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// session subscribes once and pulls until the subscription is due for
// renewal or fails. subscribed reports whether a subscription was made.
func (c *Collector) session(ctx context.Context, j *job) (subscribed bool, err error) {
	t := j.target
	transport := c.Transport
	if transport == nil {
		transport = onvif.ProbeTransport()
	}
	devEP, err := onvif.ParseEndpoint(t.DeviceURL)
	if err != nil {
		return false, err
	}
	pull := c.PullTimeout
	if pull <= 0 {
		pull = 10 * time.Second
	}
	var creds *onvif.Credentials
	if t.Username != "" {
		creds = onvif.NewCredentials(t.Username, t.Password)
	}
	client := onvif.NewClient(transport, onvif.Config{Timeout: pull + 10*time.Second, MaxBodyBytes: 1 << 20, MaxAttempts: 1, Credentials: creds})
	var offset time.Duration
	unauth := onvif.NewDeviceClient(onvif.NewClient(transport, onvif.Config{Timeout: 5 * time.Second}), devEP)
	if clk, cerr := unauth.GetSystemDateAndTime(ctx); cerr == nil && clk.UTC.Date.Year >= 2000 {
		cam := time.Date(clk.UTC.Date.Year, time.Month(clk.UTC.Date.Month), clk.UTC.Date.Day, clk.UTC.Time.Hour, clk.UTC.Time.Minute, clk.UTC.Time.Second, 0, time.UTC)
		offset = cam.Sub(c.now().UTC())
		client.SetTimeOffset(offset)
	}
	dev := onvif.NewDeviceClient(client, devEP)
	services, err := dev.GetServices(ctx)
	if err != nil || len(services) == 0 {
		if services, err = dev.GetCapabilities(ctx); err != nil {
			return false, err
		}
	}
	var evEP onvif.Endpoint
	for _, s := range services {
		if strings.Contains(s.Namespace, "/ver10/events/") {
			if evEP, err = s.ValidatedEndpoint(); err == nil {
				break
			}
		}
	}
	if evEP.String() == "" {
		return false, errors.New("the camera advertises no ONVIF events service")
	}
	ev := onvif.NewEventsClient(client, evEP)
	renew := c.Resubscribe
	if renew <= 0 {
		renew = 4 * time.Minute
	}
	term := "PT" + strconv.Itoa(int((renew+time.Minute)/time.Second)) + "S"
	sub, err := ev.CreatePullPointSubscription(ctx, term)
	if err != nil {
		return false, err
	}
	subEP, err := onvif.ParseEndpoint(strings.TrimSpace(sub.SubscriptionAddress))
	if err != nil {
		return false, errors.New("the camera returned an invalid subscription address")
	}
	c.set(j, func(s *Status) { s.State, s.LastError = "subscribed", "" })
	c.Log.Info("onvif events subscribed", "camera", t.CameraID)
	deadline := time.Now().Add(renew)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return true, nil
		}
		msgs, err := ev.PullMessages(ctx, subEP, int(pull/time.Second), 50)
		if err != nil {
			return true, err
		}
		for _, m := range msgs {
			e, ok := Classify(m)
			if !ok {
				continue
			}
			at := m.Timestamp.Add(-offset)
			if m.Timestamp.IsZero() || at.Sub(c.now()).Abs() > 10*time.Minute {
				at = c.now()
			}
			e.CameraID, e.At = t.CameraID, at.UTC()
			inserted, ierr := c.insert(ctx, e)
			if ierr != nil {
				c.Log.Warn("onvif event insert", "camera", t.CameraID, "err", ierr)
				continue
			}
			if inserted {
				c.set(j, func(s *Status) { s.LastEvent = e.At; s.Events++ })
			}
		}
	}
	return true, nil
}

// Event is one row of the events table.
type Event struct {
	CameraID string
	At       time.Time
	Type     string
	Class    string
	Topic    string
	Data     map[string]string
	SourceID map[string]string
}

var activeKeys = []string{"ismotion", "state", "istamper", "isinside", "active", "alarm", "value", "detected", "objectsinside"}

// Classify turns an ONVIF notification into an event start. It drops
// initial-state dumps (PropertyOperation Initialized) and the end of a
// stateful event (its boolean going false). Notifications without a
// boolean (plate reads, counters) are kept as they are.
func Classify(n onvif.EventNotification) (Event, bool) {
	if strings.EqualFold(n.PropertyOperation, "Initialized") || n.Topic == "" {
		return Event{}, false
	}
	sawBool, active := false, false
	for k, v := range n.Data {
		lk := strings.ToLower(k)
		for _, a := range activeKeys {
			if lk == a {
				switch strings.ToLower(strings.TrimSpace(v)) {
				case "true", "1":
					sawBool, active = true, true
				case "false", "0":
					sawBool = true
				}
			}
		}
	}
	if sawBool && !active {
		return Event{}, false
	}
	parts := strings.Split(strings.TrimSpace(n.Topic), "/")
	for i, p := range parts {
		if _, after, ok := strings.Cut(p, ":"); ok {
			parts[i] = after // drop namespace prefixes (tns1:, tt:...)
		}
	}
	topic := strings.Join(parts, "/")
	typ := "onvif"
	if a := onvif.DetectAnalytics([]string{topic}); len(a) > 0 {
		typ = a[0].ID
	}
	class := ""
	for _, k := range []string{"PlateNumber", "LicensePlate", "Plate", "ObjectType", "ClassType", "Class"} {
		for dk, v := range n.Data {
			if strings.EqualFold(dk, k) && v != "" {
				class = v
				break
			}
		}
		if class != "" {
			break
		}
	}
	return Event{Type: typ, Class: class, Topic: topic, Data: n.Data, SourceID: n.Source}, true
}

func canon(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k + "=" + m[k] + ";")
	}
	return b.String()
}

// insert stores e; repeated notifications of the same event in the same
// second collapse through dedupe_key.
func (c *Collector) insert(ctx context.Context, e Event) (bool, error) {
	h := sha1.Sum([]byte(e.CameraID + "|" + e.Topic + "|" + canon(e.SourceID) + "|" + e.At.Truncate(time.Second).Format(time.RFC3339)))
	raw, _ := json.Marshal(map[string]any{"topic": e.Topic, "source": e.SourceID, "data": e.Data})
	if len(raw) > 4096 {
		raw = raw[:0]
	}
	res, err := c.DB.ExecContext(ctx, `INSERT OR IGNORE INTO events (id, camera_id, timestamp_utc, type, class, source, raw_ref, dedupe_key, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, uuid.NewString(), e.CameraID, e.At.UnixMilli(), e.Type, e.Class, Source, string(raw),
		hex.EncodeToString(h[:]), c.now().UnixMilli())
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}
