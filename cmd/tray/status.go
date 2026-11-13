package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// health mirrors api.HealthStatus.
type health struct {
	State            string   `json:"state"`
	Reasons          []string `json:"reasons"`
	Version          string   `json:"version"`
	UptimeSeconds    int64    `json:"uptime_seconds"`
	StreamsTotal     int      `json:"streams_total"`
	StreamsRecording int      `json:"streams_recording"`
	VolumesTotal     int      `json:"volumes_total"`
	VolumesWritable  int      `json:"volumes_writable"`
	HeadroomBytes    uint64   `json:"headroom_bytes"`
	Alerts           int      `json:"alerts"`
}

func fetchHealth(ctx context.Context, base string) (*health, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(base, "/")+"/health/status", nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return nil, fmt.Errorf("status %d", res.StatusCode)
	}
	var h health
	return &h, json.NewDecoder(res.Body).Decode(&h)
}

// view is what the tray shows for one poll.
type view struct {
	Color   [3]byte
	Title   string // first menu line
	Detail  string // second menu line
	Tooltip string
}

// render turns service state and health into tray text. svcState is ""
// when the service is not installed (recorder run by hand).
func render(svcState string, h *health, err error) view {
	if h == nil {
		switch svcState {
		case "stopped":
			return view{colorStopped, "Servicio detenido", "No se está grabando", "OpenVMS Recorder: detenido"}
		case "start_pending":
			return view{colorWarning, "Servicio iniciando…", "", "OpenVMS Recorder: iniciando"}
		case "stop_pending":
			return view{colorWarning, "Servicio deteniéndose…", "", "OpenVMS Recorder: deteniéndose"}
		}
		msg := "El recorder no responde"
		if svcState == "" {
			msg = "Recorder no encontrado"
		}
		return view{colorDown, msg, shortErr(err), "OpenVMS Recorder: " + strings.ToLower(msg)}
	}
	title := fmt.Sprintf("Grabando %d de %d cámaras", h.StreamsRecording, h.StreamsTotal)
	if h.StreamsTotal == 0 {
		title = "Sin cámaras configuradas"
	}
	detail := fmt.Sprintf("Discos: %d de %d escribibles · %s libres", h.VolumesWritable, h.VolumesTotal, gb(h.HeadroomBytes))
	if h.Alerts > 0 {
		detail += fmt.Sprintf(" · %d alertas", h.Alerts)
	}
	color := colorOK
	if h.State != "ok" {
		color = colorWarning
	}
	tip := fmt.Sprintf("OpenVMS Recorder %s\n%s\nEncendido hace %s", h.Version, title, uptime(h.UptimeSeconds))
	if len(tip) > 127 { // Windows tooltip limit
		tip = tip[:127]
	}
	return view{color, title, detail, tip}
}

func gb(b uint64) string {
	if b >= 1e12 {
		return fmt.Sprintf("%.1f TB", float64(b)/1e12)
	}
	return fmt.Sprintf("%.0f GB", float64(b)/1e9)
}

func uptime(s int64) string {
	d := time.Duration(s) * time.Second
	switch {
	case d >= 24*time.Hour:
		return fmt.Sprintf("%dd %dh", int(d.Hours())/24, int(d.Hours())%24)
	case d >= time.Hour:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
}

func shortErr(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if len(s) > 60 {
		s = s[:60] + "…"
	}
	return s
}
