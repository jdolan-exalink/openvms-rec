package onvif

import (
	"context"
	"errors"
	"sort"
	"strings"
)

// Canonical codec names used by the video helpers. ONVIF calls Motion JPEG "JPEG".
const (
	CodecH264  = "H264"
	CodecH265  = "H265"
	CodecMJPEG = "MJPEG"
	CodecMPEG4 = "MPEG4"
)

// Bitrate control modes. A device only reports them when it declares it (Media2
// ConstantBitRateSupported); otherwise the list stays empty.
const (
	BitrateModeConstant = "constant"
	BitrateModeVariable = "variable"
)

// ErrProfileNotFound means the device has no media profile with the requested token.
var ErrProfileNotFound = errors.New("ONVIF media profile not found")

// ErrNoVideoEncoder means the profile has no video encoder configuration to read or change.
var ErrNoVideoEncoder = errors.New("ONVIF media profile has no video encoder")

// Conn identifies one device's media services. Both endpoints must come from ParseEndpoint
// (or the device's own GetCapabilities result validated through Service.ValidatedEndpoint), so
// no unvalidated URL can reach the SOAP client. Media2 (Profile T) is preferred when set because
// it is the only service that exposes H.265; otherwise Media (ver10) is used. The zero Endpoint
// means "not available".
type Conn struct {
	Client *Client
	Media  Endpoint
	Media2 Endpoint
}

// NewConn validates the endpoints and builds a Conn. Pass an empty string for a missing service.
func NewConn(client *Client, mediaURL, media2URL string) (Conn, error) {
	c := Conn{Client: client}
	var err error
	if strings.TrimSpace(mediaURL) != "" {
		if c.Media, err = ParseEndpoint(mediaURL); err != nil {
			return Conn{}, err
		}
	}
	if strings.TrimSpace(media2URL) != "" {
		if c.Media2, err = ParseEndpoint(media2URL); err != nil {
			return Conn{}, err
		}
	}
	return c, c.check()
}

func (c Conn) check() error {
	if c.Client == nil || (c.Media.value == "" && c.Media2.value == "") {
		return ErrInvalidEndpoint
	}
	return nil
}

func (c Conn) useMedia2() bool { return c.Media2.value != "" }

func (c Conn) media1() *MediaClient  { return NewMediaClient(c.Client, c.Media) }
func (c Conn) media2() *Media2Client { return NewMedia2Client(c.Client, c.Media2) }

// CodecOptions is what the device declares for one codec. A zero range or an empty list means the
// device did not report that item; nothing is ever invented.
type CodecOptions struct {
	Codec            string
	Resolutions      []VideoResolution
	FrameRates       []float64 // discrete values, when the device lists them
	FPS              FloatRange
	Bitrate          IntRange // kbps
	GOP              IntRange // GOV length (keyframe interval)
	EncodingInterval IntRange
	Quality          FloatRange
	Profiles         []string
	BitrateModes     []string
}

// VideoOptions groups the per-codec options of one video encoder configuration.
type VideoOptions struct {
	Codecs []CodecOptions
}

// For returns the options of a codec (any spelling normalizeEncoding understands).
func (o VideoOptions) For(codec string) (CodecOptions, bool) {
	want := CanonicalCodec(codec)
	for _, c := range o.Codecs {
		if c.Codec == want {
			return c, true
		}
	}
	return CodecOptions{}, false
}

// CanonicalCodec maps ONVIF spellings to the canonical codec name (JPEG becomes MJPEG).
func CanonicalCodec(encoding string) string {
	e := normalizeEncoding(encoding)
	switch e {
	case "JPEG", "MJPEG":
		return CodecMJPEG
	}
	return e
}

// VideoEncoderOptions asks the device which values the video encoder of a profile supports.
func VideoEncoderOptions(ctx context.Context, conn Conn, profileToken string) (VideoOptions, error) {
	profile, err := findProfile(ctx, conn, profileToken)
	if err != nil {
		return VideoOptions{}, err
	}
	if profile.EncoderToken == "" {
		return VideoOptions{}, ErrNoVideoEncoder
	}
	return videoOptionsFor(ctx, conn, profile.EncoderToken)
}

func videoOptionsFor(ctx context.Context, conn Conn, encoderToken string) (VideoOptions, error) {
	if conn.useMedia2() {
		return media2VideoOptions(ctx, conn.media2(), encoderToken)
	}
	return media1VideoOptions(ctx, conn.media1(), encoderToken)
}

type rawRange struct {
	Min float64 `xml:"Min"`
	Max float64 `xml:"Max"`
}

func (r rawRange) ints() IntRange     { return IntRange{Min: int(r.Min), Max: int(r.Max)} }
func (r rawRange) floats() FloatRange { return FloatRange(r) }

func media2VideoOptions(ctx context.Context, m *Media2Client, encoderToken string) (VideoOptions, error) {
	content := ""
	if encoderToken != "" {
		content = "<tr2:ConfigurationToken>" + escapeXML(encoderToken) + "</tr2:ConfigurationToken>"
	}
	data, err := m.call(ctx, "GetVideoEncoderConfigurationOptions", content, ReadOnly)
	if err != nil {
		return VideoOptions{}, err
	}
	var response struct {
		Options []struct {
			GovLengthRange           string            `xml:"GovLengthRange,attr"`
			FrameRatesSupported      string            `xml:"FrameRatesSupported,attr"`
			ProfilesSupported        string            `xml:"ProfilesSupported,attr"`
			ConstantBitRateSupported string            `xml:"ConstantBitRateSupported,attr"`
			Encoding                 string            `xml:"Encoding"`
			QualityRange             rawRange          `xml:"QualityRange"`
			Resolutions              []VideoResolution `xml:"ResolutionsAvailable"`
			BitrateRange             rawRange          `xml:"BitrateRange"`
		} `xml:"Options"`
	}
	if err := decodeNamespacedOperation(data, media2Namespace, "GetVideoEncoderConfigurationOptionsResponse", &response); err != nil {
		return VideoOptions{}, &CallError{Status: StatusFailed, Code: "invalid_video_encoder_options"}
	}
	var out VideoOptions
	for _, o := range response.Options {
		codec := CanonicalCodec(o.Encoding)
		if codec == "" {
			continue
		}
		co := CodecOptions{
			Codec:       codec,
			Resolutions: o.Resolutions,
			FrameRates:  fieldsAsFloats(o.FrameRatesSupported),
			Bitrate:     o.BitrateRange.ints(),
			Quality:     o.QualityRange.floats(),
			Profiles:    strings.Fields(o.ProfilesSupported),
		}
		if len(co.FrameRates) > 0 {
			sorted := append([]float64(nil), co.FrameRates...)
			sort.Float64s(sorted)
			co.FrameRates = sorted
			co.FPS = FloatRange{Min: sorted[0], Max: sorted[len(sorted)-1]}
		}
		if gov := fieldsAsFloats(o.GovLengthRange); len(gov) > 0 {
			co.GOP = IntRange{Min: int(gov[0]), Max: int(gov[len(gov)-1])}
		}
		switch strings.ToLower(strings.TrimSpace(o.ConstantBitRateSupported)) {
		case "true", "1":
			co.BitrateModes = []string{BitrateModeConstant, BitrateModeVariable}
		case "false", "0":
			co.BitrateModes = []string{BitrateModeVariable}
		}
		out.Codecs = append(out.Codecs, co)
	}
	return out, nil
}

type rawCodecOptions struct {
	Resolutions  []VideoResolution `xml:"ResolutionsAvailable"`
	GovLength    rawRange          `xml:"GovLengthRange"`
	FrameRate    rawRange          `xml:"FrameRateRange"`
	Interval     rawRange          `xml:"EncodingIntervalRange"`
	BitrateRange rawRange          `xml:"BitrateRange"`
	H264Profiles []string          `xml:"H264ProfilesSupported"`
	Mpeg4Profile []string          `xml:"Mpeg4ProfilesSupported"`
}

func media1VideoOptions(ctx context.Context, m *MediaClient, encoderToken string) (VideoOptions, error) {
	content := ""
	if encoderToken != "" {
		content = "<trt:ConfigurationToken>" + escapeXML(encoderToken) + "</trt:ConfigurationToken>"
	}
	data, err := m.call(ctx, "GetVideoEncoderConfigurationOptions", content, ReadOnly)
	if err != nil {
		return VideoOptions{}, err
	}
	var response struct {
		Options struct {
			QualityRange rawRange         `xml:"QualityRange"`
			JPEG         *rawCodecOptions `xml:"JPEG"`
			MPEG4        *rawCodecOptions `xml:"MPEG4"`
			H264         *rawCodecOptions `xml:"H264"`
			Extension    struct {
				JPEG  *rawCodecOptions `xml:"JPEG"`
				MPEG4 *rawCodecOptions `xml:"MPEG4"`
				H264  *rawCodecOptions `xml:"H264"`
			} `xml:"Extension"`
		} `xml:"Options"`
	}
	if err := decodeMediaOperation(data, "GetVideoEncoderConfigurationOptionsResponse", &response); err != nil {
		return VideoOptions{}, &CallError{Status: StatusFailed, Code: "invalid_video_encoder_options"}
	}
	o := response.Options
	var out VideoOptions
	add := func(codec string, base, ext *rawCodecOptions) {
		if base == nil {
			return
		}
		co := CodecOptions{
			Codec:            codec,
			Resolutions:      base.Resolutions,
			FPS:              base.FrameRate.floats(),
			GOP:              base.GovLength.ints(),
			EncodingInterval: base.Interval.ints(),
			Bitrate:          base.BitrateRange.ints(),
			Quality:          o.QualityRange.floats(),
			Profiles:         append(append([]string(nil), base.H264Profiles...), base.Mpeg4Profile...),
		}
		if ext != nil && co.Bitrate.Max == 0 {
			co.Bitrate = ext.BitrateRange.ints()
		}
		out.Codecs = append(out.Codecs, co)
	}
	add(CodecH264, o.H264, o.Extension.H264)
	add(CodecMJPEG, o.JPEG, o.Extension.JPEG)
	add(CodecMPEG4, o.MPEG4, o.Extension.MPEG4)
	return out, nil
}
