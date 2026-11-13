package onvif

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
)

const media2Namespace = "http://www.onvif.org/ver20/media/wsdl"

var (
	// ErrEncodingUnsupported means the camera did not list the requested encoding among the ones
	// it can produce, so nothing must be written.
	ErrEncodingUnsupported = errors.New("the camera does not support that video encoding")
	// ErrEncodingUnchanged means the configuration already uses the requested encoding.
	ErrEncodingUnchanged = errors.New("the camera already uses that video encoding")
)

// Media2Client provides the ONVIF Media2 (Profile T) operations. H.265 is only exposed here: the
// Media1 service lists H.264, MPEG-4 and JPEG.
type Media2Client struct {
	client   *Client
	endpoint Endpoint
}

func NewMedia2Client(client *Client, endpoint Endpoint) *Media2Client {
	return &Media2Client{client: client, endpoint: endpoint}
}

// VideoRateControl2 is the Media2 rate control: the frame rate may be fractional and the bitrate
// can be constant.
type VideoRateControl2 struct {
	ConstantBitRate bool    `xml:"ConstantBitRate,attr"`
	FrameRateLimit  float64 `xml:"http://www.onvif.org/ver10/schema FrameRateLimit"`
	BitrateLimit    int     `xml:"http://www.onvif.org/ver10/schema BitrateLimit"`
}

// VideoEncoder2Configuration is a Media2 video encoder configuration. The optional multicast block
// is not carried: streaming here is unicast RTSP.
type VideoEncoder2Configuration struct {
	Token       string            `xml:"token,attr"`
	GovLength   int               `xml:"GovLength,attr"`
	Profile     string            `xml:"Profile,attr"`
	Name        string            `xml:"http://www.onvif.org/ver10/schema Name"`
	UseCount    int               `xml:"http://www.onvif.org/ver10/schema UseCount"`
	Encoding    string            `xml:"http://www.onvif.org/ver10/schema Encoding"`
	Resolution  VideoResolution   `xml:"http://www.onvif.org/ver10/schema Resolution"`
	RateControl VideoRateControl2 `xml:"http://www.onvif.org/ver10/schema RateControl"`
	Quality     float64           `xml:"http://www.onvif.org/ver10/schema Quality"`
}

type IntRange struct{ Min, Max int }

type FloatRange struct{ Min, Max float64 }

// VideoEncoder2Options is what a camera declares it can do for one encoding.
type VideoEncoder2Options struct {
	Encoding          string
	ProfilesSupported []string
	GovLength         IntRange
	FrameRates        []float64
	Resolutions       []VideoResolution
	Bitrate           IntRange
	Quality           FloatRange
}

// Media2Profile preserves a stream profile's identity and supported configuration metadata.
type Media2Profile struct {
	Token                     string                      `xml:"token,attr"`
	Fixed                     bool                        `xml:"fixed,attr"`
	Name                      string                      `xml:"http://www.onvif.org/ver10/schema Name"`
	VideoSourceConfiguration  *VideoSourceConfiguration   `xml:"http://www.onvif.org/ver10/schema VideoSourceConfiguration"`
	VideoEncoderConfiguration *VideoEncoder2Configuration `xml:"http://www.onvif.org/ver10/schema VideoEncoderConfiguration"`
	PTZConfiguration          *PTZConfiguration           `xml:"http://www.onvif.org/ver10/schema PTZConfiguration"`
}

func (m *Media2Client) GetProfiles(ctx context.Context) ([]Media2Profile, error) {
	data, err := m.call(ctx, "GetProfiles", "", ReadOnly)
	if err != nil {
		return nil, err
	}
	var response struct {
		Profiles []Media2Profile `xml:"http://www.onvif.org/ver20/media/wsdl Profiles"`
	}
	if err := decodeNamespacedOperation(data, media2Namespace, "GetProfilesResponse", &response); err != nil {
		return nil, &CallError{Status: StatusFailed, Code: "invalid_profiles"}
	}
	return response.Profiles, nil
}

func (m *Media2Client) GetStreamURI(ctx context.Context, profileToken string) (string, error) {
	content := `<tr2:Protocol>RTSP</tr2:Protocol><tr2:ProfileToken>` + escapeXML(profileToken) + `</tr2:ProfileToken>`
	data, err := m.call(ctx, "GetStreamUri", content, ReadOnly)
	if err != nil {
		return "", err
	}
	var response struct {
		URI string `xml:"http://www.onvif.org/ver20/media/wsdl Uri"`
	}
	if err := decodeNamespacedOperation(data, media2Namespace, "GetStreamUriResponse", &response); err != nil {
		return "", &CallError{Status: StatusFailed, Code: "invalid_stream_uri"}
	}
	return response.URI, nil
}

func (m *Media2Client) GetVideoEncoderConfigurations(ctx context.Context) ([]VideoEncoder2Configuration, error) {
	data, err := m.call(ctx, "GetVideoEncoderConfigurations", "", ReadOnly)
	if err != nil {
		return nil, err
	}
	var response struct {
		Configurations []VideoEncoder2Configuration `xml:"http://www.onvif.org/ver20/media/wsdl Configurations"`
	}
	if err := decodeNamespacedOperation(data, media2Namespace, "GetVideoEncoderConfigurationsResponse", &response); err != nil {
		return nil, &CallError{Status: StatusFailed, Code: "invalid_video_encoder_configurations"}
	}
	return response.Configurations, nil
}

// GetVideoEncoderConfigurationOptions asks which encodings (and their limits) a configuration can
// use. An empty configToken asks for the options of every configuration.
func (m *Media2Client) GetVideoEncoderConfigurationOptions(ctx context.Context, configToken string) ([]VideoEncoder2Options, error) {
	content := ""
	if configToken != "" {
		content = "<tr2:ConfigurationToken>" + escapeXML(configToken) + "</tr2:ConfigurationToken>"
	}
	data, err := m.call(ctx, "GetVideoEncoderConfigurationOptions", content, ReadOnly)
	if err != nil {
		return nil, err
	}
	type bounds struct {
		Min float64 `xml:"http://www.onvif.org/ver10/schema Min"`
		Max float64 `xml:"http://www.onvif.org/ver10/schema Max"`
	}
	var response struct {
		Options []struct {
			GovLengthRange      string            `xml:"GovLengthRange,attr"`
			FrameRatesSupported string            `xml:"FrameRatesSupported,attr"`
			ProfilesSupported   string            `xml:"ProfilesSupported,attr"`
			Encoding            string            `xml:"http://www.onvif.org/ver10/schema Encoding"`
			QualityRange        bounds            `xml:"http://www.onvif.org/ver10/schema QualityRange"`
			Resolutions         []VideoResolution `xml:"http://www.onvif.org/ver10/schema ResolutionsAvailable"`
			BitrateRange        bounds            `xml:"http://www.onvif.org/ver10/schema BitrateRange"`
		} `xml:"http://www.onvif.org/ver20/media/wsdl Options"`
	}
	if err := decodeNamespacedOperation(data, media2Namespace, "GetVideoEncoderConfigurationOptionsResponse", &response); err != nil {
		return nil, &CallError{Status: StatusFailed, Code: "invalid_video_encoder_options"}
	}
	out := make([]VideoEncoder2Options, 0, len(response.Options))
	for _, o := range response.Options {
		gov := fieldsAsFloats(o.GovLengthRange)
		opt := VideoEncoder2Options{
			Encoding:          strings.TrimSpace(o.Encoding),
			ProfilesSupported: strings.Fields(o.ProfilesSupported),
			FrameRates:        fieldsAsFloats(o.FrameRatesSupported),
			Resolutions:       o.Resolutions,
			Bitrate:           IntRange{Min: int(o.BitrateRange.Min), Max: int(o.BitrateRange.Max)},
			Quality:           FloatRange{Min: o.QualityRange.Min, Max: o.QualityRange.Max},
		}
		if len(gov) > 0 {
			opt.GovLength = IntRange{Min: int(gov[0]), Max: int(gov[len(gov)-1])}
		}
		out = append(out, opt)
	}
	return out, nil
}

// SetVideoEncoderConfiguration writes a whole configuration. Media2 has no ForcePersistence flag:
// the camera decides what survives a reboot.
func (m *Media2Client) SetVideoEncoderConfiguration(ctx context.Context, cfg VideoEncoder2Configuration) error {
	content := fmt.Sprintf(`<tr2:Configuration token="%s" GovLength="%d" Profile="%s" xmlns:tt="%s">`+
		`<tt:Name>%s</tt:Name>`+
		`<tt:UseCount>%d</tt:UseCount>`+
		`<tt:Encoding>%s</tt:Encoding>`+
		`<tt:Resolution><tt:Width>%d</tt:Width><tt:Height>%d</tt:Height></tt:Resolution>`+
		`<tt:RateControl ConstantBitRate="%t"><tt:FrameRateLimit>%s</tt:FrameRateLimit><tt:BitrateLimit>%d</tt:BitrateLimit></tt:RateControl>`+
		`<tt:Quality>%s</tt:Quality>`+
		`</tr2:Configuration>`,
		escapeXML(cfg.Token), cfg.GovLength, escapeXML(cfg.Profile), schemaNamespace,
		escapeXML(cfg.Name), cfg.UseCount, escapeXML(cfg.Encoding),
		cfg.Resolution.Width, cfg.Resolution.Height,
		cfg.RateControl.ConstantBitRate, formatNumber(cfg.RateControl.FrameRateLimit), cfg.RateControl.BitrateLimit,
		formatNumber(cfg.Quality))
	_, err := m.call(ctx, "SetVideoEncoderConfiguration", content, Mutating)
	return err
}

// PlanEncodingChange returns the configuration to send to switch cur to the target encoding. What
// the new encoding allows is kept (resolution, profile, frame rate, GOV length, bitrate, quality);
// what it does not is moved to the nearest value the camera declared. A target the camera did not
// list is refused so nothing unsupported is ever written. cur is not modified.
func PlanEncodingChange(cur VideoEncoder2Configuration, options []VideoEncoder2Options, target string) (VideoEncoder2Configuration, error) {
	want := normalizeEncoding(target)
	if want == normalizeEncoding(cur.Encoding) {
		return VideoEncoder2Configuration{}, ErrEncodingUnchanged
	}
	var opt *VideoEncoder2Options
	for i := range options {
		if normalizeEncoding(options[i].Encoding) == want {
			opt = &options[i]
			break
		}
	}
	if opt == nil {
		return VideoEncoder2Configuration{}, ErrEncodingUnsupported
	}

	out := cur
	out.Encoding = want
	out.Profile = pickProfile(cur.Profile, opt.ProfilesSupported)
	out.Resolution = nearestResolution(cur.Resolution, opt.Resolutions)
	out.RateControl.FrameRateLimit = snapFrameRate(cur.RateControl.FrameRateLimit, opt.FrameRates)
	out.GovLength = clampInt(cur.GovLength, opt.GovLength)
	out.RateControl.BitrateLimit = clampInt(cur.RateControl.BitrateLimit, opt.Bitrate)
	out.Quality = clampFloat(cur.Quality, opt.Quality)
	return out, nil
}

// normalizeEncoding spells an encoding the way ONVIF does: H.265 and HEVC are "H265".
func normalizeEncoding(encoding string) string {
	e := strings.ToUpper(strings.TrimSpace(encoding))
	e = strings.NewReplacer(".", "", "-", "", "_", "", " ", "").Replace(e)
	if e == "HEVC" {
		return "H265"
	}
	return e
}

func pickProfile(current string, supported []string) string {
	if len(supported) == 0 {
		return current
	}
	for _, p := range supported {
		if strings.EqualFold(p, current) {
			return p
		}
	}
	for _, p := range supported {
		if strings.EqualFold(p, "Main") {
			return p
		}
	}
	return supported[0]
}

func nearestResolution(current VideoResolution, available []VideoResolution) VideoResolution {
	if len(available) == 0 {
		return current
	}
	best := available[0]
	for _, r := range available {
		if r == current {
			return current
		}
		d, bd := pixelDistance(r, current), pixelDistance(best, current)
		if d < bd || (d == bd && r.Width*r.Height > best.Width*best.Height) {
			best = r
		}
	}
	return best
}

func pixelDistance(a, b VideoResolution) int {
	d := a.Width*a.Height - b.Width*b.Height
	if d < 0 {
		return -d
	}
	return d
}

// snapFrameRate keeps the current rate when offered, else the highest offered one below it, else
// the lowest offered.
func snapFrameRate(current float64, supported []float64) float64 {
	if len(supported) == 0 {
		return current
	}
	best, lowest := math.NaN(), supported[0]
	for _, f := range supported {
		if f == current {
			return current
		}
		if f < lowest {
			lowest = f
		}
		if f < current && (math.IsNaN(best) || f > best) {
			best = f
		}
	}
	if math.IsNaN(best) {
		return lowest
	}
	return best
}

func clampInt(v int, r IntRange) int {
	if r.Max <= 0 {
		return v
	}
	return min(max(v, r.Min), r.Max)
}

func clampFloat(v float64, r FloatRange) float64 {
	if r.Max <= 0 {
		return v
	}
	return math.Min(math.Max(v, r.Min), r.Max)
}

func fieldsAsFloats(list string) []float64 {
	var out []float64
	for _, f := range strings.Fields(list) {
		if v, err := strconv.ParseFloat(f, 64); err == nil {
			out = append(out, v)
		}
	}
	return out
}

func formatNumber(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

func escapeXML(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

func decodeNamespacedOperation(data []byte, namespace, name string, target any) error {
	decoder := xml.NewDecoder(bytes.NewReader(data))
	for {
		token, err := decoder.Token()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return fmt.Errorf("missing SOAP operation %s", name)
			}
			return err
		}
		start, ok := token.(xml.StartElement)
		if !ok || start.Name.Space != namespace || start.Name.Local != name {
			continue
		}
		return decoder.DecodeElement(target, &start)
	}
}

func (m *Media2Client) call(ctx context.Context, action, content string, op Operation) ([]byte, error) {
	body := `<tr2:` + action + ` xmlns:tr2="` + media2Namespace + `">` + content + `</tr2:` + action + `>`
	return m.client.Call(ctx, m.endpoint, media2Namespace+"/"+action, body, op)
}
