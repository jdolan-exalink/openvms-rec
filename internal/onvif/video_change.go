package onvif

import (
	"fmt"
	"math"
	"strings"
)

// VideoDesired is a partial video change: nil/empty fields are left as they are. CurrentCodec is
// the codec the profile uses today; it is only consulted to check resolution, FPS and the other
// codec-dependent fields when Codec itself is not being changed.
type VideoDesired struct {
	Codec        string
	Resolution   *VideoResolution
	FPS          *float64
	BitrateKbps  *int
	BitrateMode  string
	GOP          *int
	Quality      *float64
	CurrentCodec string
}

// Violation codes.
const (
	ViolationOptionsUnavailable = "options_unavailable"
	ViolationUnsupportedCodec   = "unsupported_codec"
	ViolationUnsupportedRes     = "unsupported_resolution"
	ViolationFPSOutOfRange      = "fps_out_of_range"
	ViolationBitrateOutOfRange  = "bitrate_out_of_range"
	ViolationGOPOutOfRange      = "gop_out_of_range"
	ViolationQualityOutOfRange  = "quality_out_of_range"
	ViolationUnsupportedMode    = "unsupported_bitrate_mode"
	ViolationInvalidValue       = "invalid_value"
)

// Violation is one broken constraint. Field is one of codec, resolution, fps, bitrate,
// bitrate_mode, gop, quality.
type Violation struct {
	Field   string
	Code    string
	Message string
}

// ValidationError carries every violation of a rejected change.
type ValidationError struct{ Violations []Violation }

func (e *ValidationError) Error() string {
	parts := make([]string, 0, len(e.Violations))
	for _, v := range e.Violations {
		parts = append(parts, v.Field+": "+v.Code)
	}
	return "video change not supported by the device: " + strings.Join(parts, ", ")
}

// ValidateVideoChange returns every constraint the desired change breaks against what the device
// declared. A value is only rejected when the device reported the corresponding limit; an item the
// device did not report is not checked (and never invented).
func ValidateVideoChange(options VideoOptions, desired VideoDesired) []Violation {
	var out []Violation
	add := func(field, code, format string, args ...any) {
		out = append(out, Violation{Field: field, Code: code, Message: fmt.Sprintf(format, args...)})
	}
	if len(options.Codecs) == 0 {
		return []Violation{{Field: "codec", Code: ViolationOptionsUnavailable,
			Message: "El dispositivo no informó qué valores de video soporta."}}
	}
	codec := CanonicalCodec(desired.Codec)
	if codec == "" {
		codec = CanonicalCodec(desired.CurrentCodec)
	}
	co, known := options.For(codec)
	if desired.Codec != "" && !known {
		add("codec", ViolationUnsupportedCodec, "El códec %s no está soportado por el dispositivo.", codec)
	}
	if desired.Resolution != nil {
		r := *desired.Resolution
		switch {
		case r.Width <= 0 || r.Height <= 0:
			add("resolution", ViolationInvalidValue, "La resolución debe ser positiva.")
		case known && len(co.Resolutions) > 0 && !hasResolution(co.Resolutions, r):
			add("resolution", ViolationUnsupportedRes, "La resolución %dx%d no está soportada con %s.", r.Width, r.Height, codec)
		}
	}
	if desired.FPS != nil {
		f := *desired.FPS
		switch {
		case f <= 0 || math.IsNaN(f) || math.IsInf(f, 0):
			add("fps", ViolationInvalidValue, "Los FPS deben ser positivos.")
		case known && len(co.FrameRates) > 0 && !hasFloat(co.FrameRates, f):
			add("fps", ViolationFPSOutOfRange, "%s FPS no está soportado con %s.", formatNumber(f), codec)
		case known && len(co.FrameRates) == 0 && co.FPS.Max > 0 && (f < co.FPS.Min || f > co.FPS.Max):
			add("fps", ViolationFPSOutOfRange, "Los FPS deben estar entre %s y %s.", formatNumber(co.FPS.Min), formatNumber(co.FPS.Max))
		}
	}
	if desired.BitrateKbps != nil {
		b := *desired.BitrateKbps
		switch {
		case b <= 0:
			add("bitrate", ViolationInvalidValue, "El bitrate debe ser positivo.")
		case known && co.Bitrate.Max > 0 && (b < co.Bitrate.Min || b > co.Bitrate.Max):
			add("bitrate", ViolationBitrateOutOfRange, "El bitrate debe estar entre %d y %d kbps.", co.Bitrate.Min, co.Bitrate.Max)
		}
	}
	if desired.GOP != nil {
		g := *desired.GOP
		switch {
		case g <= 0:
			add("gop", ViolationInvalidValue, "El GOP debe ser positivo.")
		case known && co.GOP.Max > 0 && (g < co.GOP.Min || g > co.GOP.Max):
			add("gop", ViolationGOPOutOfRange, "El GOP debe estar entre %d y %d.", co.GOP.Min, co.GOP.Max)
		}
	}
	if desired.Quality != nil {
		q := *desired.Quality
		if known && co.Quality.Max > 0 && (q < co.Quality.Min || q > co.Quality.Max) {
			add("quality", ViolationQualityOutOfRange, "La calidad debe estar entre %s y %s.", formatNumber(co.Quality.Min), formatNumber(co.Quality.Max))
		}
	}
	if desired.BitrateMode != "" {
		mode := strings.ToLower(strings.TrimSpace(desired.BitrateMode))
		switch {
		case mode != BitrateModeConstant && mode != BitrateModeVariable:
			add("bitrate_mode", ViolationInvalidValue, "Modo de bitrate desconocido.")
		case known && len(co.BitrateModes) > 0 && !containsString(co.BitrateModes, mode):
			add("bitrate_mode", ViolationUnsupportedMode, "El modo de bitrate %s no está soportado con %s.", mode, codec)
		case known && len(co.BitrateModes) == 0:
			add("bitrate_mode", ViolationUnsupportedMode, "El dispositivo no informa el modo de bitrate.")
		}
	}
	return out
}

// Impact kinds. Only stream restarts are derivable from ONVIF parameters alone.
const ImpactStreamRestart = "stream_restart"

// StreamRestartMessage is shown before saving a change that restarts the stream.
const StreamRestartMessage = "Este cambio reiniciará el stream durante aproximadamente algunos segundos."

// Impact tells what a change will disturb before it is saved.
type Impact struct {
	Kind    string
	Message string
	Fields  []string
}

// ClassifyImpact lists the side effects of applying desired on top of current. A field equal to
// the current value is not a change.
func ClassifyImpact(current ProfileStream, desired VideoDesired) []Impact {
	var fields []string
	if desired.Codec != "" && CanonicalCodec(desired.Codec) != CanonicalCodec(current.Codec) {
		fields = append(fields, "codec")
	}
	if desired.Resolution != nil && *desired.Resolution != current.Resolution {
		fields = append(fields, "resolution")
	}
	if desired.FPS != nil && math.Abs(*desired.FPS-current.FPS) > 1e-6 {
		fields = append(fields, "fps")
	}
	if desired.BitrateKbps != nil && *desired.BitrateKbps != current.BitrateKbps {
		fields = append(fields, "bitrate")
	}
	if desired.GOP != nil && *desired.GOP != current.GOP {
		fields = append(fields, "gop")
	}
	if len(fields) == 0 {
		return nil
	}
	return []Impact{{Kind: ImpactStreamRestart, Message: StreamRestartMessage, Fields: fields}}
}

func hasResolution(list []VideoResolution, r VideoResolution) bool {
	for _, c := range list {
		if c == r {
			return true
		}
	}
	return false
}

func hasFloat(list []float64, v float64) bool {
	for _, c := range list {
		if math.Abs(c-v) < 1e-6 {
			return true
		}
	}
	return false
}

func containsString(list []string, v string) bool {
	for _, c := range list {
		if c == v {
			return true
		}
	}
	return false
}
