package onvif

import (
	"context"
	"strconv"
	"strings"
)

// ProfileStream is the current video setup of one media profile plus whether the profile carries
// an audio encoder. Zero values mean the device did not report the item.
type ProfileStream struct {
	Token           string
	Name            string
	EncoderToken    string
	Codec           string
	Resolution      VideoResolution
	FPS             float64
	BitrateKbps     int
	BitrateMode     string // BitrateModeConstant, BitrateModeVariable or empty
	GOP             int
	Quality         float64
	HasAudioEncoder bool
	Service         string // "media1" or "media2"

	// Fields kept so a write can send the configuration back without losing them.
	encoderName      string
	useCount         int
	codecProfile     string
	encodingInterval int
}

type rawVideoEncoder struct {
	Token      string `xml:"token,attr"`
	GovAttr    int    `xml:"GovLength,attr"`
	ProfAttr   string `xml:"Profile,attr"`
	Name       string `xml:"Name"`
	UseCount   int    `xml:"UseCount"`
	Encoding   string `xml:"Encoding"`
	Resolution struct {
		Width  int `xml:"Width"`
		Height int `xml:"Height"`
	} `xml:"Resolution"`
	Quality     float64 `xml:"Quality"`
	RateControl struct {
		ConstantBitRate  string  `xml:"ConstantBitRate,attr"`
		FrameRateLimit   float64 `xml:"FrameRateLimit"`
		EncodingInterval int     `xml:"EncodingInterval"`
		BitrateLimit     int     `xml:"BitrateLimit"`
	} `xml:"RateControl"`
	H264 *struct {
		GovLength   int    `xml:"GovLength"`
		H264Profile string `xml:"H264Profile"`
	} `xml:"H264"`
}

type rawAudio struct {
	Token string `xml:"token,attr"`
}

type rawProfile struct {
	Token          string `xml:"token,attr"`
	Name           string `xml:"Name"`
	Configurations *struct {
		Video *rawVideoEncoder `xml:"VideoEncoder"`
		Audio *rawAudio        `xml:"AudioEncoder"`
	} `xml:"Configurations"`
	Video *rawVideoEncoder `xml:"VideoEncoderConfiguration"`
	Audio *rawAudio        `xml:"AudioEncoderConfiguration"`
}

func (p rawProfile) toStream(service string) ProfileStream {
	s := ProfileStream{Token: p.Token, Name: strings.TrimSpace(p.Name), Service: service}
	v, a := p.Video, p.Audio
	if p.Configurations != nil {
		if p.Configurations.Video != nil {
			v = p.Configurations.Video
		}
		if p.Configurations.Audio != nil {
			a = p.Configurations.Audio
		}
	}
	s.HasAudioEncoder = a != nil
	if v == nil {
		return s
	}
	s.EncoderToken = v.Token
	s.encoderName = v.Name
	s.useCount = v.UseCount
	s.Codec = CanonicalCodec(v.Encoding)
	s.Resolution = VideoResolution{Width: v.Resolution.Width, Height: v.Resolution.Height}
	s.FPS = v.RateControl.FrameRateLimit
	s.BitrateKbps = v.RateControl.BitrateLimit
	s.encodingInterval = v.RateControl.EncodingInterval
	s.Quality = v.Quality
	s.GOP = v.GovAttr
	s.codecProfile = v.ProfAttr
	if v.H264 != nil {
		s.GOP = v.H264.GovLength
		s.codecProfile = v.H264.H264Profile
	}
	if b, err := strconv.ParseBool(strings.TrimSpace(v.RateControl.ConstantBitRate)); err == nil {
		s.BitrateMode = BitrateModeVariable
		if b {
			s.BitrateMode = BitrateModeConstant
		}
	}
	return s
}

// ProfileStreams returns the current video settings of every profile that has a video encoder,
// and whether each carries an audio encoder.
func ProfileStreams(ctx context.Context, conn Conn) ([]ProfileStream, error) {
	if err := conn.check(); err != nil {
		return nil, err
	}
	raw, service, err := fetchRawProfiles(ctx, conn)
	if err != nil {
		return nil, err
	}
	out := make([]ProfileStream, 0, len(raw))
	for _, p := range raw {
		out = append(out, p.toStream(service))
	}
	return out, nil
}

// fetchRawProfiles reads the stream profiles and the media service that produced them. It prefers
// Media2, asking for every configuration with Type=All. Some cameras (observed on Provision-ISR
// firmware) answer that Media2 call with a SOAP fault even though they serve Media1; when a second
// service is available the read is retried there, because Media1 GetProfiles returns the full video
// encoder configuration with no Type parameter. It falls back only on error, and a device offering
// a single service keeps its original behaviour and error.
func fetchRawProfiles(ctx context.Context, conn Conn) ([]rawProfile, string, error) {
	if conn.useMedia2() {
		raw, err := media2Profiles(ctx, conn)
		if err == nil {
			return raw, "media2", nil
		}
		if conn.Media.value == "" {
			return nil, "media2", err
		}
		if raw, merr := media1Profiles(ctx, conn); merr == nil {
			return raw, "media1", nil
		}
		return nil, "media2", err
	}
	raw, err := media1Profiles(ctx, conn)
	return raw, "media1", err
}

func media2Profiles(ctx context.Context, conn Conn) ([]rawProfile, error) {
	data, err := conn.media2().call(ctx, "GetProfiles", "<tr2:Type>All</tr2:Type>", ReadOnly)
	if err != nil {
		return nil, err
	}
	var response struct {
		Profiles []rawProfile `xml:"Profiles"`
	}
	if err := decodeNamespacedOperation(data, media2Namespace, "GetProfilesResponse", &response); err != nil {
		return nil, &CallError{Status: StatusFailed, Code: "invalid_profiles"}
	}
	return response.Profiles, nil
}

func media1Profiles(ctx context.Context, conn Conn) ([]rawProfile, error) {
	data, err := conn.media1().call(ctx, "GetProfiles", "", ReadOnly)
	if err != nil {
		return nil, err
	}
	var response struct {
		Profiles []rawProfile `xml:"Profiles"`
	}
	if err := decodeMediaOperation(data, "GetProfilesResponse", &response); err != nil {
		return nil, &CallError{Status: StatusFailed, Code: "invalid_profiles"}
	}
	return response.Profiles, nil
}

func findProfile(ctx context.Context, conn Conn, profileToken string) (ProfileStream, error) {
	streams, err := ProfileStreams(ctx, conn)
	if err != nil {
		return ProfileStream{}, err
	}
	for _, s := range streams {
		if s.Token == profileToken {
			return s, nil
		}
	}
	return ProfileStream{}, ErrProfileNotFound
}
