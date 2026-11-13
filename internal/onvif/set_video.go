package onvif

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
)

// ErrGOPNotWritable means the Media (ver10) service cannot carry a GOP for the requested codec.
var ErrGOPNotWritable = errors.New("the device cannot change the GOP for that codec through ONVIF Media")

// VerifyError means the device accepted the change but the re-read configuration still differs.
type VerifyError struct{ Fields []string }

func (e *VerifyError) Error() string {
	return "the device did not apply: " + strings.Join(e.Fields, ", ")
}

// SetVideoEncoder changes only the requested fields of a profile's video encoder. It reads the
// current configuration and the device options, refuses combinations the device does not declare
// (*ValidationError), sends the full configuration back and re-reads it to verify. It returns the
// re-read stream; on a mismatch it also returns a *VerifyError. SOAP and transport failures are
// the sanitized *CallError of the SOAP client, which never contains credentials.
func SetVideoEncoder(ctx context.Context, conn Conn, profileToken string, desired VideoDesired) (ProfileStream, error) {
	cur, err := findProfile(ctx, conn, profileToken)
	if err != nil {
		return ProfileStream{}, err
	}
	if cur.EncoderToken == "" {
		return ProfileStream{}, ErrNoVideoEncoder
	}
	desired.CurrentCodec = cur.Codec
	options, err := videoOptionsFor(ctx, conn, cur.EncoderToken)
	if err != nil {
		return ProfileStream{}, err
	}
	if v := ValidateVideoChange(options, desired); len(v) > 0 {
		return ProfileStream{}, &ValidationError{Violations: v}
	}
	next := applyDesired(cur, options, desired)
	if conn.useMedia2() {
		err = conn.media2().SetVideoEncoderConfiguration(ctx, toMedia2Config(next))
	} else {
		if desired.GOP != nil && next.Codec != CodecH264 {
			return ProfileStream{}, ErrGOPNotWritable
		}
		err = setMedia1(ctx, conn.media1(), next)
	}
	if err != nil {
		return ProfileStream{}, fmt.Errorf("set video encoder: %w", err)
	}
	got, err := findProfile(ctx, conn, profileToken)
	if err != nil {
		return ProfileStream{}, fmt.Errorf("verify video encoder: %w", err)
	}
	if diff := verifyApplied(next, got, desired); len(diff) > 0 {
		return got, &VerifyError{Fields: diff}
	}
	return got, nil
}

// applyDesired copies cur and overwrites only the requested fields.
func applyDesired(cur ProfileStream, options VideoOptions, d VideoDesired) ProfileStream {
	next := cur
	if d.Codec != "" && CanonicalCodec(d.Codec) != cur.Codec {
		next.Codec = CanonicalCodec(d.Codec)
		next.codecProfile = ""
		if co, ok := options.For(next.Codec); ok {
			next.codecProfile = pickProfile("", co.Profiles)
		}
	}
	if d.Resolution != nil {
		next.Resolution = *d.Resolution
	}
	if d.FPS != nil {
		next.FPS = *d.FPS
	}
	if d.BitrateKbps != nil {
		next.BitrateKbps = *d.BitrateKbps
	}
	if d.BitrateMode != "" {
		next.BitrateMode = strings.ToLower(strings.TrimSpace(d.BitrateMode))
	}
	if d.GOP != nil {
		next.GOP = *d.GOP
	}
	if d.Quality != nil {
		next.Quality = *d.Quality
	}
	return next
}

func onvifEncoding(codec string) string {
	if codec == CodecMJPEG {
		return "JPEG"
	}
	return codec
}

func toMedia2Config(s ProfileStream) VideoEncoder2Configuration {
	return VideoEncoder2Configuration{
		Token: s.EncoderToken, GovLength: s.GOP, Profile: s.codecProfile,
		Name: s.encoderName, UseCount: s.useCount, Encoding: onvifEncoding(s.Codec),
		Resolution: s.Resolution,
		RateControl: VideoRateControl2{
			ConstantBitRate: s.BitrateMode == BitrateModeConstant,
			FrameRateLimit:  s.FPS, BitrateLimit: s.BitrateKbps,
		},
		Quality: s.Quality,
	}
}

// setMedia1 writes a ver10 configuration, keeping the H.264 GOP block that the generic
// MediaClient.SetVideoEncoderConfiguration does not carry.
func setMedia1(ctx context.Context, m *MediaClient, s ProfileStream) error {
	interval := s.encodingInterval
	if interval <= 0 {
		interval = 1
	}
	var codecBlock string
	if s.Codec == CodecH264 && s.GOP > 0 {
		codecBlock = `<tt:H264><tt:GovLength>` + fmt.Sprint(s.GOP) + `</tt:GovLength>`
		if s.codecProfile != "" {
			codecBlock += `<tt:H264Profile>` + escapeXML(s.codecProfile) + `</tt:H264Profile>`
		}
		codecBlock += `</tt:H264>`
	}
	content := `<trt:Configuration token="` + escapeXML(s.EncoderToken) + `" xmlns:tt="` + schemaNamespace + `">` +
		`<tt:Name>` + escapeXML(s.encoderName) + `</tt:Name>` +
		`<tt:UseCount>` + fmt.Sprint(s.useCount) + `</tt:UseCount>` +
		`<tt:Encoding>` + escapeXML(onvifEncoding(s.Codec)) + `</tt:Encoding>` +
		`<tt:Resolution><tt:Width>` + fmt.Sprint(s.Resolution.Width) + `</tt:Width><tt:Height>` + fmt.Sprint(s.Resolution.Height) + `</tt:Height></tt:Resolution>` +
		`<tt:Quality>` + formatNumber(s.Quality) + `</tt:Quality>` +
		`<tt:RateControl><tt:FrameRateLimit>` + fmt.Sprint(int(math.Round(s.FPS))) + `</tt:FrameRateLimit>` +
		`<tt:EncodingInterval>` + fmt.Sprint(interval) + `</tt:EncodingInterval>` +
		`<tt:BitrateLimit>` + fmt.Sprint(s.BitrateKbps) + `</tt:BitrateLimit></tt:RateControl>` +
		codecBlock +
		`</trt:Configuration><trt:ForcePersistence>true</trt:ForcePersistence>`
	_, err := m.call(ctx, "SetVideoEncoderConfiguration", content, Mutating)
	return err
}

// verifyApplied lists the requested fields whose re-read value differs from what was sent.
func verifyApplied(sent, got ProfileStream, d VideoDesired) []string {
	var diff []string
	if d.Codec != "" && got.Codec != sent.Codec {
		diff = append(diff, "codec")
	}
	if d.Resolution != nil && got.Resolution != sent.Resolution {
		diff = append(diff, "resolution")
	}
	if d.FPS != nil && math.Abs(got.FPS-sent.FPS) >= 0.5 {
		diff = append(diff, "fps")
	}
	if d.BitrateKbps != nil && got.BitrateKbps != sent.BitrateKbps {
		diff = append(diff, "bitrate")
	}
	if d.GOP != nil && got.GOP != sent.GOP {
		diff = append(diff, "gop")
	}
	if d.BitrateMode != "" && got.BitrateMode != "" && got.BitrateMode != sent.BitrateMode {
		diff = append(diff, "bitrate_mode")
	}
	return diff
}
