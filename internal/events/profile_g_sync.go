package events

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jdolan-exalink/openvms-rec/internal/cameras"
	"github.com/jdolan-exalink/openvms-rec/internal/config"
	"github.com/jdolan-exalink/openvms-rec/internal/ingest"
	"github.com/jdolan-exalink/openvms-rec/internal/onvif"
	"github.com/jdolan-exalink/openvms-rec/internal/segments"
	"github.com/jdolan-exalink/openvms-rec/internal/storage"
)

// ProfileGSyncResult summarizes an ONVIF Profile G microSD gap backfill run.
type ProfileGSyncResult struct {
	JobID             string `json:"job_id,omitempty"`
	CameraID          string `json:"camera_id"`
	Status            string `json:"status"`
	SummaryRecordings int    `json:"summary_recordings,omitempty"`
	RecordingsFound   int    `json:"recordings_found"`
	EarliestRecording string `json:"earliest_recording,omitempty"`
	LatestRecording   string `json:"latest_recording,omitempty"`
	ReplayURI         string `json:"replay_uri,omitempty"`
	GapsEvaluated     int    `json:"gaps_evaluated"`
	SegmentsRecovered int    `json:"segments_recovered"`
	BytesRecovered    uint64 `json:"bytes_recovered"`
	CheckedAtUTC      string `json:"checked_at_utc,omitempty"`
	Message           string `json:"message,omitempty"`
}

// ReplayFetcher downloads a bounded RTSP replay clip via Profile G URI to dstPath.
type ReplayFetcher func(ctx context.Context, ffmpegPath, replayRTSPURL string, from, to time.Time, maxMbps float64, dstPath string) error

// ProfileGSyncService performs bounded, non-blocking microSD gap recovery for
// cameras with verified ONVIF Profile G Search and Replay capabilities (ADR-003).
type ProfileGSyncService struct {
	DB        *sql.DB
	Events    *Store
	Segments  *segments.Store
	Cameras   *cameras.Registry
	Storage   *storage.Manager
	Cfg       config.Recovery
	FFmpeg    string
	Transport http.RoundTripper
	Fetch     ReplayFetcher
	Log       *slog.Logger

	mu            sync.Mutex
	running       map[string]bool
	last          map[string]ProfileGSyncResult
	unsupportedAt map[string]time.Time
	notifyCh      chan string
}

// NotifyCamera wakes up StartLoop to evaluate microSD backfill for cameraID as soon as a recording gap closes.
func (s *ProfileGSyncService) NotifyCamera(cameraID string) {
	if s == nil || cameraID == "" {
		return
	}
	s.mu.Lock()
	if s.notifyCh == nil {
		s.notifyCh = make(chan string, 32)
	}
	ch := s.notifyCh
	s.mu.Unlock()
	select {
	case ch <- cameraID:
	default:
	}
}

// StartSync launches or runs a bounded Profile G recovery check for cameraID.
// Unlike live recording, backfill never invents continuity: only gaps matching
// verified edge recordings and real downloaded media are imported.
func (s *ProfileGSyncService) RunCameraSync(ctx context.Context, cameraID string, lookback time.Duration) (ProfileGSyncResult, error) {
	if s == nil || s.Cameras == nil || s.Segments == nil || s.Storage == nil {
		return ProfileGSyncResult{}, errors.New("profile g sync not configured")
	}
	if strings.EqualFold(s.Cfg.CameraSDBackfill, "disabled") {
		return ProfileGSyncResult{}, errors.New("camera_sd_backfill is disabled in configuration")
	}
	cam, ok := s.Cameras.Get(cameraID)
	if !ok {
		return ProfileGSyncResult{}, fmt.Errorf("camera %q not found", cameraID)
	}
	host := cam.EffectiveHost()
	if host == "" {
		return ProfileGSyncResult{}, fmt.Errorf("camera %q has no host configured for ONVIF", cameraID)
	}

	s.mu.Lock()
	if s.running == nil {
		s.running = map[string]bool{}
	}
	if s.running[cameraID] {
		s.mu.Unlock()
		return ProfileGSyncResult{}, errors.New("sd sync already running for camera")
	}
	s.running[cameraID] = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.running, cameraID)
		s.mu.Unlock()
	}()

	jobID := NewEventID()
	s.saveJob(ctx, jobID, "running", map[string]any{"camera_id": cameraID}, 0, "")

	res, err := s.executeSync(ctx, jobID, cam, host, lookback)
	nowStr := time.Now().UTC().Format(time.RFC3339)
	if res.CameraID == "" {
		res.CameraID = cameraID
	}
	if res.JobID == "" {
		res.JobID = jobID
	}
	res.CheckedAtUTC = nowStr
	if err != nil {
		res.Status = "failed"
		res.Message = err.Error()
		s.mu.Lock()
		if s.last == nil {
			s.last = map[string]ProfileGSyncResult{}
		}
		s.last[cameraID] = res
		if strings.Contains(strings.ToLower(err.Error()), "does not advertise onvif profile g") {
			if s.unsupportedAt == nil {
				s.unsupportedAt = map[string]time.Time{}
			}
			s.unsupportedAt[cameraID] = time.Now().UTC()
		}
		s.mu.Unlock()
		dbCtx, dbCancel := context.WithTimeout(context.Background(), 5*time.Second)
		s.saveJob(dbCtx, jobID, "failed", res, 0, err.Error())
		dbCancel()
		return res, err
	}
	s.mu.Lock()
	if s.last == nil {
		s.last = map[string]ProfileGSyncResult{}
	}
	s.last[cameraID] = res
	if s.unsupportedAt != nil {
		delete(s.unsupportedAt, cameraID)
	}
	s.mu.Unlock()
	s.saveJob(ctx, jobID, "succeeded", res, 1, "")
	return res, nil
}

func validSDTime(t time.Time) bool {
	return !t.IsZero() && t.Year() >= 2000
}

func (s *ProfileGSyncService) executeSync(ctx context.Context, jobID string, cam cameras.Entry, host string, lookback time.Duration) (ProfileGSyncResult, error) {
	out := ProfileGSyncResult{JobID: jobID, CameraID: cam.ID, Status: "succeeded"}
	port := cam.EffectiveONVIFPort()
	if port <= 0 {
		port = 80
	}

	var creds *onvif.Credentials
	var user, pass string
	if c, ok := s.Cameras.Credentials(cam.ID); ok && c.Username != "" {
		creds = onvif.NewCredentials(c.Username, c.Password)
		user, pass = c.Username, c.Password
	}
	client := onvif.NewClient(s.Transport, onvif.Config{
		Timeout:     8 * time.Second,
		Credentials: creds,
	})
	devEP, err := onvif.EndpointForHost(host, port)
	if err != nil {
		return out, err
	}
	dev := onvif.NewDeviceClient(client, devEP)
	if clk, clkErr := dev.GetSystemDateAndTime(ctx); clkErr == nil {
		if camUTC := clk.Time(); !camUTC.IsZero() {
			client.SetTimeOffset(camUTC.Sub(time.Now().UTC()))
		}
	}

	svcs, err := dev.GetServices(ctx)
	if err != nil {
		svcs, err = dev.GetCapabilities(ctx)
		if err != nil {
			return out, fmt.Errorf("onvif services: %w", err)
		}
	}
	var searchEP, replayEP, recEP *onvif.Endpoint
	for _, sv := range svcs {
		ns := strings.ToLower(sv.Namespace)
		ep, epErr := onvif.RewriteToHost(sv.Endpoint, host, port)
		if epErr != nil {
			continue
		}
		if strings.Contains(ns, "/search") {
			e := ep
			searchEP = &e
		} else if strings.Contains(ns, "/replay") {
			e := ep
			replayEP = &e
		} else if strings.Contains(ns, "/recording") {
			e := ep
			recEP = &e
		}
	}
	if searchEP == nil || replayEP == nil {
		if s.Events != nil {
			ev, _ := json.Marshal(map[string]any{"op": "GetServices", "search": searchEP != nil, "replay": replayEP != nil})
			_ = s.Events.SaveCapabilities(ctx, cam.ID, []onvif.Capability{{
				Capability: "onvif.profile_g",
				Status:     onvif.CapUnsupported,
				Evidence:   string(ev),
				CheckedAt:  time.Now().UTC(),
			}})
		}
		return out, errors.New("camera does not advertise ONVIF Profile G search/replay services")
	}

	sc := onvif.NewSearchClient(client, *searchEP)
	rc := onvif.NewReplayClient(client, *replayEP)
	summary, sumErr := sc.GetRecordingSummary(ctx)
	if sumErr == nil {
		out.SummaryRecordings = summary.NumberRecordings
		if validSDTime(summary.DataFrom) {
			out.EarliestRecording = summary.DataFrom.UTC().Format(time.RFC3339)
		}
		if validSDTime(summary.DataUntil) {
			out.LatestRecording = summary.DataUntil.UTC().Format(time.RFC3339)
		}
	}
	recs, err := sc.ListRecordings(ctx, 20)
	if err != nil && len(recs) == 0 && recEP != nil {
		if gr, gErr := onvif.NewRecordingClient(client, *recEP).GetRecordings(ctx); gErr == nil && len(gr) > 0 {
			recs = gr
			err = nil
		}
	} else if len(recs) == 0 && recEP != nil {
		if gr, gErr := onvif.NewRecordingClient(client, *recEP).GetRecordings(ctx); gErr == nil && len(gr) > 0 {
			recs = gr
		}
	}
	var jobNote string
	if recEP != nil {
		if jobs, jErr := onvif.NewRecordingClient(client, *recEP).GetRecordingJobs(ctx); jErr == nil && len(jobs) > 0 {
			var jp []string
			for _, j := range jobs {
				jp = append(jp, fmt.Sprintf("%s=%s(%s)", j.JobToken, j.Mode, j.State))
			}
			jobNote = "jobs: " + strings.Join(jp, ", ")
		}
	}
	if err != nil && len(recs) == 0 {
		if sumErr == nil && summary.NumberRecordings == 0 {
			out.Message = "no recordings found on edge storage (microSD empty or unformatted)"
			if jobNote != "" {
				out.Message += "; " + jobNote
			}
			return out, nil
		}
		return out, fmt.Errorf("profile g search: %w", err)
	}
	out.RecordingsFound = len(recs)
	if len(recs) == 0 {
		out.Message = "no recordings found on edge storage"
		if jobNote != "" {
			out.Message += "; " + jobNote
		}
		return out, nil
	}

	var rawReplayURI string
	var chosenRec onvif.RecordingInfo
	for _, r := range recs {
		uri, uErr := rc.GetReplayURI(ctx, r.RecordingToken)
		if uErr == nil && uri != "" {
			rawReplayURI = uri
			chosenRec = r
			break
		}
	}
	if !validSDTime(chosenRec.EarliestRecording) {
		chosenRec.EarliestRecording = time.Time{}
	}
	if !validSDTime(chosenRec.LatestRecording) {
		chosenRec.LatestRecording = time.Time{}
	}
	if chosenRec.EarliestRecording.IsZero() && sumErr == nil && validSDTime(summary.DataFrom) {
		chosenRec.EarliestRecording = summary.DataFrom
	}
	if chosenRec.LatestRecording.IsZero() && sumErr == nil && validSDTime(summary.DataUntil) {
		chosenRec.LatestRecording = summary.DataUntil
	}
	if validSDTime(chosenRec.EarliestRecording) {
		out.EarliestRecording = chosenRec.EarliestRecording.UTC().Format(time.RFC3339)
	}
	if validSDTime(chosenRec.LatestRecording) {
		out.LatestRecording = chosenRec.LatestRecording.UTC().Format(time.RFC3339)
	}
	if rawReplayURI == "" {
		return out, errors.New("profile g replay did not return a usable RTSP URI")
	}
	out.ReplayURI = onvif.SanitizeRTSPURI(rawReplayURI)
	if s.Events != nil {
		ev, _ := json.Marshal(map[string]any{"op": "GetReplayUri", "recording_token": chosenRec.RecordingToken})
		_ = s.Events.SaveCapabilities(ctx, cam.ID, []onvif.Capability{
			{Capability: "onvif.profile_g.search", Status: onvif.CapConfirmed, Evidence: string(ev), CheckedAt: time.Now().UTC()},
			{Capability: "onvif.profile_g.replay", Status: onvif.CapConfirmed, Evidence: string(ev), CheckedAt: time.Now().UTC()},
			{Capability: "onvif.profile_g", Status: onvif.CapConfirmed, Evidence: string(ev), CheckedAt: time.Now().UTC()},
		})
	}

	// If ONVIF returned no valid recording dates (e.g. 1970-01-01 sentinel), check TVT /GetDiskInfo
	// read-only to distinguish an unformatted/empty microSD card from actual recordings.
	if !validSDTime(chosenRec.EarliestRecording) && !validSDTime(chosenRec.LatestRecording) {
		if disk, ok := ProbeTVTDisk(ctx, s.Transport, host, port, user, pass); ok {
			if strings.EqualFold(disk.Status, "unformat") {
				if disk.TotalSpace == 0 {
					out.Message = "sin microSD insertada o tarjeta sin formatear (status=unformat)"
				} else {
					out.Message = "microSD detectada en cámara pero sin formatear (status=unformat); formateala y habilitá grabación en la web de la cámara"
				}
				return out, nil
			}
			if disk.Count == 0 || disk.TotalSpace == 0 {
				out.Message = fmt.Sprintf("microSD sin capacidad utilizable (count=%d, status=%s)", disk.Count, disk.Status)
				return out, nil
			}
		}
	}

	if strings.EqualFold(s.Cfg.CameraSDBackfill, "index_remote_only") {
		out.Message = fmt.Sprintf("indexed %d Profile G recording tracks (index_remote_only mode)", len(recs))
		return out, nil
	}

	if lookback <= 0 {
		lookback = 24 * time.Hour
	}
	to := time.Now().UTC()
	from := to.Add(-lookback)
	gaps, err := s.Segments.Gaps(ctx, cam.ID, "main", from, to)
	if err != nil {
		return out, err
	}

	authReplayURL := injectCredsIntoRTSP(rawReplayURI, user, pass)
	fetcher := s.Fetch
	if fetcher == nil {
		fetcher = defaultReplayFetcher
	}

	const maxBackfillChunk = 120 * time.Second
	const maxChunksPerPass = 10
	chunksRun := 0

	// Process newest gaps first so older unrecoverable gaps never starve recent cuts.
	for i, j := 0, len(gaps)-1; i < j; i, j = i+1, j-1 {
		gaps[i], gaps[j] = gaps[j], gaps[i]
	}

	for _, g := range gaps {
		if ctx.Err() != nil || chunksRun >= maxChunksPerPass {
			break
		}
		if g.End.IsZero() || !g.End.After(g.Start) {
			continue
		}

		// Intersect gap against the camera's reported recording bounds if available.
		recFrom, recTo := g.Start, g.End
		if validSDTime(chosenRec.EarliestRecording) && chosenRec.EarliestRecording.After(recFrom) {
			recFrom = chosenRec.EarliestRecording
		}
		if validSDTime(chosenRec.LatestRecording) && chosenRec.LatestRecording.Before(recTo) {
			recTo = chosenRec.LatestRecording
		}
		if !recTo.After(recFrom) || recTo.Sub(recFrom) < 2*time.Second {
			continue
		}
		out.GapsEvaluated++

		// Fetch gap in bounded chunks so long outages never starve live recording.
		contigEnd := recFrom
		for cur := recFrom; cur.Before(recTo) && chunksRun < maxChunksPerPass; {
			if ctx.Err() != nil {
				break
			}
			chunkEnd := cur.Add(maxBackfillChunk)
			if chunkEnd.After(recTo) {
				chunkEnd = recTo
			}
			if chunkEnd.Sub(cur) < 2*time.Second {
				break
			}
			// Avoid duplicating ranges already covered in local segments.
			if existing, _ := s.Segments.Range(ctx, cam.ID, "main", cur, chunkEnd); len(existing) > 0 {
				break
			}
			dec, root, err := s.Storage.Choose(cam.ID, 16<<20)
			if err != nil {
				if contigEnd.After(recFrom) {
					_ = s.Segments.ResolveGapCoverage(ctx, g.ID, recFrom, contigEnd)
				}
				return out, fmt.Errorf("choose volume for recovery: %w", err)
			}
			rel := ingest.RelativePath(cam.ID, "main", cur)
			seg := segments.Segment{
				CameraID:         cam.ID,
				RepresentationID: "main",
				Start:            cur,
				VolumeID:         dec.VolumeID,
				RelativePath:     rel,
				Source:           segments.SourceRecoveredSD,
			}
			segID, err := s.Segments.Begin(ctx, seg)
			if err != nil && strings.Contains(err.Error(), "UNIQUE constraint") {
				rel = strings.TrimSuffix(rel, ".ts") + "_sd_" + NewEventID()[4:10] + ".ts"
				seg.RelativePath = rel
				segID, err = s.Segments.Begin(ctx, seg)
			}
			if err != nil {
				if contigEnd.After(recFrom) {
					_ = s.Segments.ResolveGapCoverage(ctx, g.ID, recFrom, contigEnd)
				}
				return out, err
			}
			tmp := ingest.TempPath(root, segID)
			_ = os.MkdirAll(filepath.Dir(tmp), 0o750)
			final := filepath.Join(root, filepath.FromSlash(rel))
			_ = os.MkdirAll(filepath.Dir(final), 0o750)
			chunksRun++

			if err := fetcher(ctx, s.FFmpeg, authReplayURL, cur, chunkEnd, s.Cfg.MaxBandwidthMbps, tmp); err != nil {
				_ = os.Remove(tmp)
				dbCtx, dbCancel := context.WithTimeout(context.Background(), 5*time.Second)
				_ = s.Segments.MarkCorrupt(dbCtx, segID, 0)
				dbCancel()
				if s.Log != nil {
					s.Log.Warn("profile g clip recovery failed", "camera", cam.ID, "gap_id", g.ID, "err", err)
				}
				break
			}
			fi, err := os.Stat(tmp)
			if err != nil || fi.Size() == 0 {
				_ = os.Remove(tmp)
				dbCtx, dbCancel := context.WithTimeout(context.Background(), 5*time.Second)
				_ = s.Segments.MarkCorrupt(dbCtx, segID, 0)
				dbCancel()
				break
			}
			sum, err := hashFileSHA256(tmp)
			if err != nil {
				_ = os.Remove(tmp)
				dbCtx, dbCancel := context.WithTimeout(context.Background(), 5*time.Second)
				_ = s.Segments.MarkCorrupt(dbCtx, segID, 0)
				dbCancel()
				break
			}
			if err := os.Rename(tmp, final); err != nil {
				_ = os.Remove(tmp)
				dbCtx, dbCancel := context.WithTimeout(context.Background(), 5*time.Second)
				_ = s.Segments.MarkCorrupt(dbCtx, segID, 0)
				dbCancel()
				if contigEnd.After(recFrom) {
					_ = s.Segments.ResolveGapCoverage(ctx, g.ID, recFrom, contigEnd)
				}
				return out, err
			}
			if err := s.Segments.Finalize(ctx, segID, chunkEnd, uint64(fi.Size()), sum, ""); err != nil {
				if contigEnd.After(recFrom) {
					_ = s.Segments.ResolveGapCoverage(ctx, g.ID, recFrom, contigEnd)
				}
				return out, err
			}
			contigEnd = chunkEnd
			out.SegmentsRecovered++
			out.BytesRecovered += uint64(fi.Size())
			cur = chunkEnd
		}
		if contigEnd.After(recFrom) {
			_ = s.Segments.ResolveGapCoverage(ctx, g.ID, recFrom, contigEnd)
		}
	}
	return out, nil
}

// LastResult returns the most recent Profile G sync outcome recorded for
// cameraID in memory or from the persistent jobs table.
func (s *ProfileGSyncService) LastResult(ctx context.Context, cameraID string) (ProfileGSyncResult, bool) {
	if s == nil || cameraID == "" {
		return ProfileGSyncResult{}, false
	}
	s.mu.Lock()
	if r, ok := s.last[cameraID]; ok {
		s.mu.Unlock()
		return r, true
	}
	s.mu.Unlock()
	if s.DB == nil {
		return ProfileGSyncResult{}, false
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT id, state, payload, error, updated_at FROM jobs WHERE kind = 'sd_recovery' ORDER BY updated_at DESC LIMIT 20`)
	if err != nil {
		return ProfileGSyncResult{}, false
	}
	defer rows.Close()
	for rows.Next() {
		var id, state, payloadStr, errStr string
		var upd int64
		if rows.Scan(&id, &state, &payloadStr, &errStr, &upd) != nil {
			continue
		}
		var res ProfileGSyncResult
		if json.Unmarshal([]byte(payloadStr), &res) != nil || res.CameraID != cameraID {
			continue
		}
		if res.JobID == "" {
			res.JobID = id
		}
		if res.Status == "" {
			res.Status = state
		}
		if res.Message == "" && errStr != "" {
			res.Message = errStr
		}
		if res.CheckedAtUTC == "" && upd > 0 {
			res.CheckedAtUTC = time.UnixMilli(upd).UTC().Format(time.RFC3339)
		}
		return res, true
	}
	return ProfileGSyncResult{}, false
}

// StartLoop periodically checks enabled cameras with closed recording gaps and
// invokes ONVIF Profile G recovery without disturbing live ingestion. When
// NotifyCamera signals a newly closed gap, recovery runs after a brief settle delay.
func (s *ProfileGSyncService) StartLoop(ctx context.Context, interval time.Duration) {
	if s == nil || s.Cameras == nil || s.Segments == nil || interval <= 0 {
		return
	}
	if strings.EqualFold(s.Cfg.CameraSDBackfill, "disabled") {
		return
	}
	s.mu.Lock()
	if s.notifyCh == nil {
		s.notifyCh = make(chan string, 32)
	}
	ch := s.notifyCh
	s.mu.Unlock()

	initDelay := min(interval, 20*time.Second)
	select {
	case <-time.After(initDelay):
	case camID := <-ch:
		s.runTriggeredBatch(ctx, ch, camID)
	case <-ctx.Done():
		return
	}
	s.runOnceAll(ctx)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case camID := <-ch:
			s.runTriggeredBatch(ctx, ch, camID)
		case <-t.C:
			s.runOnceAll(ctx)
		}
	}
}

func (s *ProfileGSyncService) runTriggeredBatch(ctx context.Context, ch chan string, firstCam string) {
	pending := map[string]struct{}{firstCam: {}}
	// Brief settle window so camera edge index/flush settles and duplicate triggers coalesce.
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
drainLoop:
	for {
		select {
		case <-ctx.Done():
			return
		case cid := <-ch:
			pending[cid] = struct{}{}
		case <-timer.C:
			break drainLoop
		}
	}
	now := time.Now().UTC()
	for camID := range pending {
		if ctx.Err() != nil {
			return
		}
		s.mu.Lock()
		until := s.unsupportedAt[camID]
		s.mu.Unlock()
		if !until.IsZero() && now.Sub(until) < 30*time.Minute {
			continue
		}
		if s.Log != nil {
			s.Log.Info("auto profile g sd-sync triggered after gap recovery", "camera", camID)
		}
		runCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		res, err := s.RunCameraSync(runCtx, camID, 24*time.Hour)
		cancel()
		if err != nil {
			if s.Log != nil {
				s.Log.Debug("auto profile g sd-sync after gap finished with err", "camera", camID, "err", err)
			}
			continue
		}
		if s.Log != nil {
			s.Log.Info("auto profile g sd-sync completed", "camera", camID,
				"gaps_evaluated", res.GapsEvaluated, "segments_recovered", res.SegmentsRecovered, "message", res.Message)
		}
	}
}

func (s *ProfileGSyncService) runOnceAll(ctx context.Context) {
	now := time.Now().UTC()
	from := now.Add(-24 * time.Hour)
	for _, cam := range s.Cameras.List() {
		if ctx.Err() != nil {
			return
		}
		if !cam.IsEnabled() || cam.EffectiveHost() == "" {
			continue
		}
		s.mu.Lock()
		until := s.unsupportedAt[cam.ID]
		s.mu.Unlock()
		if !until.IsZero() && now.Sub(until) < 30*time.Minute {
			continue
		}
		gaps, err := s.Segments.Gaps(ctx, cam.ID, "main", from, now)
		if err != nil {
			continue
		}
		hasClosedGap := false
		// Process newest gaps first so older unrecoverable gaps never starve recent cuts.
		for i, j := 0, len(gaps)-1; i < j; i, j = i+1, j-1 {
			gaps[i], gaps[j] = gaps[j], gaps[i]
		}

		for _, g := range gaps {
			if !g.End.IsZero() && g.End.Sub(g.Start) >= 2*time.Second {
				hasClosedGap = true
				break
			}
		}
		if !hasClosedGap {
			continue
		}
		runCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		res, err := s.RunCameraSync(runCtx, cam.ID, 24*time.Hour)
		cancel()
		if err != nil {
			if s.Log != nil && !strings.Contains(strings.ToLower(err.Error()), "does not advertise onvif profile g") {
				s.Log.Debug("auto profile g sd-sync skipped", "camera", cam.ID, "err", err)
			}
			continue
		}
		if res.SegmentsRecovered > 0 && s.Log != nil {
			s.Log.Info("auto profile g sd-sync recovered gaps", "camera", cam.ID, "segments", res.SegmentsRecovered, "bytes", res.BytesRecovered)
		}
	}
}

func defaultReplayFetcher(ctx context.Context, ffmpegPath, replayRTSPURL string, from, to time.Time, maxMbps float64, dstPath string) error {
	if ffmpegPath == "" {
		return errors.New("ffmpeg path required")
	}
	dur := to.Sub(from)
	if dur <= 0 {
		return errors.New("invalid replay range")
	}
	_ = maxMbps

	inputURL := replayRTSPURL
	cleanup := func() {}
	if strings.HasPrefix(strings.ToLower(replayRTSPURL), "rtsp://") {
		if pu, stop, pErr := startRTSPRangeProxy(ctx, replayRTSPURL, from, to); pErr == nil {
			inputURL, cleanup = pu, stop
		}
	}
	defer cleanup()

	attemptTimeout := dur + 15*time.Second
	if attemptTimeout < 18*time.Second {
		attemptTimeout = 18 * time.Second
	} else if attemptTimeout > 60*time.Second {
		attemptTimeout = 60 * time.Second
	}

	runCopy := func(mediaTypes string, audioArgs []string) error {
		subCtx, cancel := context.WithTimeout(ctx, attemptTimeout)
		defer cancel()
		args := []string{
			"-nostdin", "-hide_banner", "-loglevel", "error",
			"-rtsp_transport", "tcp",
			"-allowed_media_types", mediaTypes,
			"-timeout", "10000000",
			"-fflags", "+genpts+discardcorrupt",
			"-analyzeduration", "2000000",
			"-probesize", "1048576",
			"-i", inputURL,
			"-t", strconv.FormatFloat(dur.Seconds(), 'f', 3, 64),
			"-sn", "-dn",
		}
		args = append(args, audioArgs...)
		args = append(args, "-f", "mpegts", "-y", dstPath)
		out, err := exec.CommandContext(subCtx, ffmpegPath, args...).CombinedOutput()
		if err != nil {
			if subCtx.Err() == context.DeadlineExceeded && ctx.Err() == nil {
				return fmt.Errorf("ffmpeg replay timed out after %s: %w", attemptTimeout, subCtx.Err())
			}
			msg := sanitizeRTSPError(strings.TrimSpace(string(out)))
			if msg != "" {
				return fmt.Errorf("%w: %s", err, msg)
			}
			return err
		}
		return nil
	}
	err := runCopy("video+audio", []string{"-c", "copy"})
	if err == nil {
		return nil
	}
	lower := strings.ToLower(err.Error())
	if strings.Contains(lower, "453 ") || strings.Contains(lower, "404 ") || strings.Contains(lower, "401 ") {
		return err
	}
	// Cameras using G.711 audio may either fail MPEG-TS muxing with -c copy or
	// advertise an inactive `pcma/0` audio track in Profile G SDP that stalls probing.
	if !errors.Is(err, context.DeadlineExceeded) {
		if err2 := runCopy("video+audio", []string{"-map", "0:v:0", "-map", "0:a:0?", "-c:v", "copy", "-c:a", "aac", "-b:a", "64k"}); err2 == nil {
			return nil
		}
	}
	return runCopy("video", []string{"-map", "0:v:0", "-c:v", "copy"})
}

func sanitizeRTSPError(msg string) string {
	var out strings.Builder
	rest := msg
	for {
		idx := strings.Index(strings.ToLower(rest), "rtsp://")
		if idx < 0 {
			out.WriteString(rest)
			return out.String()
		}
		out.WriteString(rest[:idx+7])
		after := rest[idx+7:]
		at := strings.IndexByte(after, '@')
		slash := strings.IndexAny(after, "/ \r\n\t")
		if at >= 0 && (slash < 0 || at < slash) {
			out.WriteString("***@")
			rest = after[at+1:]
		} else {
			rest = after
		}
	}
}

func injectCredsIntoRTSP(rawURL, user, pass string) string {
	if user == "" {
		return rawURL
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.User != nil {
		return rawURL
	}
	u.User = url.UserPassword(user, pass)
	return u.String()
}

func hashFileSHA256(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (s *ProfileGSyncService) saveJob(ctx context.Context, id, state string, payload any, progress float64, errMsg string) {
	if s.DB == nil {
		return
	}
	pb, _ := json.Marshal(payload)
	now := time.Now().UTC().UnixMilli()
	_, _ = s.DB.ExecContext(ctx, `INSERT INTO jobs(id, kind, state, payload, progress, error, created_at, updated_at)
		VALUES (?, 'sd_recovery', ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET state = excluded.state, payload = excluded.payload,
		progress = excluded.progress, error = excluded.error, updated_at = excluded.updated_at`,
		id, state, string(pb), progress, errMsg, now, now)
}
