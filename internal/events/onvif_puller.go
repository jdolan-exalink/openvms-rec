package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/jdolan-exalink/openvms-rec/internal/cameras"
	"github.com/jdolan-exalink/openvms-rec/internal/onvif"
)

// ONVIFPuller subscribes to ONVIF PullPoint events for enabled cameras and
// stores normalized detections in Store.
type ONVIFPuller struct {
	Store     *Store
	Cameras   *cameras.Registry
	Transport http.RoundTripper
	Log       *slog.Logger

	mu      sync.Mutex
	runCtx  context.Context
	workers map[string]context.CancelFunc
	wg      sync.WaitGroup
}

func NewONVIFPuller(store *Store, cams *cameras.Registry, log *slog.Logger) *ONVIFPuller {
	if log == nil {
		log = slog.Default()
	}
	return &ONVIFPuller{
		Store:   store,
		Cameras: cams,
		Log:     log,
		workers: make(map[string]context.CancelFunc),
	}
}

// Start begins polling all eligible cameras and blocks until ctx is canceled.
func (p *ONVIFPuller) Start(ctx context.Context) {
	p.mu.Lock()
	p.runCtx = ctx
	p.mu.Unlock()

	p.Sync()
	<-ctx.Done()

	p.mu.Lock()
	for id, cancel := range p.workers {
		cancel()
		delete(p.workers, id)
	}
	p.mu.Unlock()
	p.wg.Wait()
}

// Sync reconciles running PullPoint workers with the current camera registry.
func (p *ONVIFPuller) Sync() {
	if p == nil || p.Cameras == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.runCtx == nil || p.runCtx.Err() != nil {
		return
	}

	desired := map[string]cameras.Entry{}
	for _, e := range p.Cameras.List() {
		if e.IsEnabled() && e.PollONVIFEvents() && e.EffectiveHost() != "" {
			desired[e.ID] = e
		}
	}

	for id, cancel := range p.workers {
		if _, ok := desired[id]; !ok {
			cancel()
			delete(p.workers, id)
		}
	}
	for id, e := range desired {
		if _, ok := p.workers[id]; !ok {
			wctx, cancel := context.WithCancel(p.runCtx)
			p.workers[id] = cancel
			p.wg.Add(1)
			go func(entry cameras.Entry) {
				defer p.wg.Done()
				p.runCamera(wctx, entry)
			}(e)
		}
	}
}

// RestartCamera restarts the PullPoint worker for a single camera after update.
func (p *ONVIFPuller) RestartCamera(id string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	if cancel, ok := p.workers[id]; ok {
		cancel()
		delete(p.workers, id)
	}
	p.mu.Unlock()
	p.Sync()
}

func (p *ONVIFPuller) runCamera(ctx context.Context, e cameras.Entry) {
	host := e.EffectiveHost()
	port := e.EffectiveONVIFPort()
	backoff := 5 * time.Second

	for ctx.Err() == nil {
		err := p.pollLoop(ctx, e, host, port)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			backoff = 5 * time.Second
			continue
		}
		p.Log.Warn("onvif pullpoint retry", "camera", e.ID, "backoff", backoff, "err", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 60*time.Second)
	}
}

func (p *ONVIFPuller) pollLoop(ctx context.Context, e cameras.Entry, host string, port int) error {
	var creds *onvif.Credentials
	if c, ok := p.Cameras.Credentials(e.ID); ok && c.Username != "" {
		creds = onvif.NewCredentials(c.Username, c.Password)
	}
	tr := p.Transport
	if tr == nil {
		tr = &http.Transport{
			DisableKeepAlives: true,
		}
	}
	client := onvif.NewClient(tr, onvif.Config{
		Timeout:     15 * time.Second,
		Credentials: creds,
	})
	devEP, err := onvif.EndpointForHost(host, port)
	if err != nil {
		return err
	}
	dev := onvif.NewDeviceClient(client, devEP)
	if clk, clkErr := dev.GetSystemDateAndTime(ctx); clkErr == nil {
		if camUTC := clk.Time(); !camUTC.IsZero() {
			client.SetTimeOffset(camUTC.Sub(time.Now().UTC()))
		}
	}

	evEP := devEP
	if svcs, svcErr := dev.GetServices(ctx); svcErr == nil {
		for _, s := range svcs {
			if containsFold(s.Namespace, "/events") {
				if ep, epErr := onvif.RewriteToHost(s.Endpoint, host, port); epErr == nil {
					evEP = ep
				}
				break
			}
		}
	}

	evClient := onvif.NewEventsClient(client, evEP)
	subAddr, err := evClient.CreatePullPointSubscription(ctx, "PT2M")
	if err != nil {
		return err
	}
	subEP, err := onvif.RewriteToHost(subAddr, host, port)
	if err != nil {
		return err
	}
	defer func() {
		uCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = evClient.Unsubscribe(uCtx, subEP)
	}()

	evJSON, _ := json.Marshal(map[string]string{"op": "CreatePullPointSubscription"})
	_ = p.Store.SaveCapabilities(ctx, e.ID, []onvif.Capability{{
		Capability: "onvif.events.pullpoint",
		Status:     onvif.CapConfirmed,
		Evidence:   string(evJSON),
		CheckedAt:  time.Now().UTC(),
	}})

	lastSeen := map[string]time.Time{}
	subRenew := time.Now().Add(90 * time.Second)
	transientErrs := 0

	for ctx.Err() == nil && time.Now().Before(subRenew) {
		notifs, err := evClient.PullMessages(ctx, subEP, 5, 20)
		if err != nil {
			var ce *onvif.CallError
			if errors.As(err, &ce) && ce.Status == onvif.StatusDegraded && transientErrs < 3 {
				transientErrs++
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(500 * time.Millisecond):
				}
				continue
			}
			return err
		}
		transientErrs = 0
		now := time.Now().UTC()
		for _, n := range notifs {
			cls := onvif.ClassifyNotification(n)
			if !cls.Active || cls.Type == "" {
				if cls.Type == "" && n.Topic != "" {
					p.Log.Info("onvif notification ignored", "camera", e.ID, "topic", n.Topic, "data", n.Data)
				}
				continue
			}
			// When local NVR ROI motion tracking is active, skip blind boolean ONVIF
			// CellMotionDetector events that carry no bounding box.
			if cls.Type == "motion" && len(cls.BBox) == 0 && len(cls.Detections) == 0 && e.NVRMotion {
				continue
			}
			ts := n.Timestamp.UTC()
			if ts.IsZero() || ts.Year() < 2000 {
				ts = now
			}
			key := cls.Type + "|" + cls.Class + "|" + cls.Zone
			if prev, ok := lastSeen[key]; ok && now.Sub(prev) < 5*time.Second {
				continue
			}
			lastSeen[key] = now

			dedupeKey := fmt.Sprintf("onvif:%s:%s:%s:%d", e.ID, cls.Type, cls.Class, ts.Unix()/5)
			var dets []Detection
			for _, d := range cls.Detections {
				detType := cls.Type
				if d.Class != "" {
					detType = d.Class
				}
				dets = append(dets, Detection{
					Type:       detType,
					Class:      d.Class,
					Confidence: d.Confidence,
					BBox:       d.BBox,
				})
			}
			inserted, insErr := p.Store.Insert(ctx, Event{
				CameraID:     e.ID,
				TimestampUTC: ts,
				Type:         cls.Type,
				Class:        cls.Class,
				Confidence:   cls.Confidence,
				BBox:         cls.BBox,
				Detections:   dets,
				Zone:         cls.Zone,
				Source:       "onvif_events",
			}, dedupeKey)
			if insErr == nil && inserted {
				p.Log.Info("onvif event recorded", "camera", e.ID, "type", cls.Type, "class", cls.Class, "zone", cls.Zone)
				capEv, _ := json.Marshal(map[string]string{"op": "PullMessages", "topic": n.Topic})
				_ = p.Store.SaveCapabilities(ctx, e.ID, []onvif.Capability{{
					Capability: "analytics." + cls.Type,
					Status:     onvif.CapConfirmed,
					Evidence:   string(capEv),
					CheckedAt:  now,
				}})
			}
		}
	}
	return nil
}

func containsFold(s, sub string) bool {
	if len(sub) > len(s) {
		return false
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		equal := true
		for j := 0; j < len(sub); j++ {
			a, b := s[i+j], sub[j]
			if a >= 'A' && a <= 'Z' {
				a += 'a' - 'A'
			}
			if b >= 'A' && b <= 'Z' {
				b += 'a' - 'A'
			}
			if a != b {
				equal = false
				break
			}
		}
		if equal {
			return true
		}
	}
	return false
}
