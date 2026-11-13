package onvif

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
)

const mediaNamespace = "http://www.onvif.org/ver10/media/wsdl"

// MediaClient provides ONVIF Media service operations.
type MediaClient struct {
	client   *Client
	endpoint Endpoint
}

func NewMediaClient(client *Client, endpoint Endpoint) *MediaClient {
	return &MediaClient{client: client, endpoint: endpoint}
}

type Profile struct {
	Token                     string                     `xml:"token,attr"`
	Fixed                     bool                       `xml:"fixed,attr"`
	Name                      string                     `xml:"http://www.onvif.org/ver10/schema Name"`
	VideoSourceConfiguration  *VideoSourceConfiguration  `xml:"http://www.onvif.org/ver10/schema VideoSourceConfiguration"`
	VideoEncoderConfiguration *VideoEncoderConfiguration `xml:"http://www.onvif.org/ver10/schema VideoEncoderConfiguration"`
	PTZConfiguration          *PTZConfiguration          `xml:"http://www.onvif.org/ver10/schema PTZConfiguration"`
}

type VideoSourceConfiguration struct {
	Token       string `xml:"token,attr"`
	SourceToken string `xml:"http://www.onvif.org/ver10/schema SourceToken"`
}

type VideoEncoderConfiguration struct {
	Token       string           `xml:"token,attr"`
	Name        string           `xml:"http://www.onvif.org/ver10/schema Name"`
	UseCount    int              `xml:"http://www.onvif.org/ver10/schema UseCount"`
	Encoding    string           `xml:"http://www.onvif.org/ver10/schema Encoding"`
	Resolution  VideoResolution  `xml:"http://www.onvif.org/ver10/schema Resolution"`
	Quality     float64          `xml:"http://www.onvif.org/ver10/schema Quality"`
	RateControl VideoRateControl `xml:"http://www.onvif.org/ver10/schema RateControl"`
}

type VideoResolution struct {
	Width  int `xml:"http://www.onvif.org/ver10/schema Width"`
	Height int `xml:"http://www.onvif.org/ver10/schema Height"`
}

type VideoRateControl struct {
	FrameRateLimit   int `xml:"http://www.onvif.org/ver10/schema FrameRateLimit"`
	EncodingInterval int `xml:"http://www.onvif.org/ver10/schema EncodingInterval"`
	BitrateLimit     int `xml:"http://www.onvif.org/ver10/schema BitrateLimit"`
}

type PTZConfiguration struct {
	Token     string `xml:"token,attr"`
	NodeToken string `xml:"http://www.onvif.org/ver10/schema NodeToken"`
}

type MediaURI struct {
	URI                 string `xml:"http://www.onvif.org/ver10/schema Uri"`
	InvalidAfterConnect bool   `xml:"http://www.onvif.org/ver10/schema InvalidAfterConnect"`
	InvalidAfterReboot  bool   `xml:"http://www.onvif.org/ver10/schema InvalidAfterReboot"`
	Timeout             string `xml:"http://www.onvif.org/ver10/schema Timeout"`
}

func (m *MediaClient) GetProfiles(ctx context.Context) ([]Profile, error) {
	data, err := m.call(ctx, "GetProfiles", "", ReadOnly)
	if err != nil {
		return nil, err
	}
	var response struct {
		Profiles []Profile `xml:"http://www.onvif.org/ver10/media/wsdl Profiles"`
	}
	if err := decodeMediaOperation(data, "GetProfilesResponse", &response); err != nil {
		return nil, &CallError{Status: StatusFailed, Code: "invalid_profiles"}
	}
	return response.Profiles, nil
}

func (m *MediaClient) GetStreamURI(ctx context.Context, profileToken string) (string, error) {
	content := `<trt:StreamSetup>` +
		`<tt:Stream xmlns:tt="http://www.onvif.org/ver10/schema">RTP-Unicast</tt:Stream>` +
		`<tt:Transport xmlns:tt="http://www.onvif.org/ver10/schema"><tt:Protocol>RTSP</tt:Protocol></tt:Transport>` +
		`</trt:StreamSetup>` +
		`<trt:ProfileToken>` + profileToken + `</trt:ProfileToken>`

	data, err := m.call(ctx, "GetStreamUri", content, ReadOnly)
	if err != nil {
		return "", err
	}
	var response struct {
		MediaURI MediaURI `xml:"http://www.onvif.org/ver10/media/wsdl MediaUri"`
	}
	if err := decodeMediaOperation(data, "GetStreamUriResponse", &response); err != nil {
		return "", &CallError{Status: StatusFailed, Code: "invalid_stream_uri"}
	}
	return response.MediaURI.URI, nil
}

// GetSnapshotURI returns the camera-provided snapshot URI for a media profile.
// Callers must treat the URI as untrusted device data and must not follow it without
// applying their own destination and credential-safety policy.
func (m *MediaClient) GetSnapshotURI(ctx context.Context, profileToken string) (string, error) {
	content := `<trt:ProfileToken>` + escapeXML(profileToken) + `</trt:ProfileToken>`
	data, err := m.call(ctx, "GetSnapshotUri", content, ReadOnly)
	if err != nil {
		return "", err
	}
	var response struct {
		MediaURI MediaURI `xml:"http://www.onvif.org/ver10/media/wsdl MediaUri"`
	}
	if err := decodeMediaOperation(data, "GetSnapshotUriResponse", &response); err != nil {
		return "", &CallError{Status: StatusFailed, Code: "invalid_snapshot_uri"}
	}
	return response.MediaURI.URI, nil
}

func (m *MediaClient) GetVideoEncoderConfigurations(ctx context.Context) ([]VideoEncoderConfiguration, error) {
	data, err := m.call(ctx, "GetVideoEncoderConfigurations", "", ReadOnly)
	if err != nil {
		return nil, err
	}
	var response struct {
		Configurations []VideoEncoderConfiguration `xml:"http://www.onvif.org/ver10/media/wsdl Configurations"`
	}
	if err := decodeMediaOperation(data, "GetVideoEncoderConfigurationsResponse", &response); err != nil {
		return nil, &CallError{Status: StatusFailed, Code: "invalid_video_encoder_configurations"}
	}
	return response.Configurations, nil
}

func (m *MediaClient) SetVideoEncoderConfiguration(ctx context.Context, cfg VideoEncoderConfiguration) error {
	content := fmt.Sprintf(`<trt:Configuration token="%s">`+
		`<tt:Name xmlns:tt="http://www.onvif.org/ver10/schema">%s</tt:Name>`+
		`<tt:UseCount xmlns:tt="http://www.onvif.org/ver10/schema">%d</tt:UseCount>`+
		`<tt:Encoding xmlns:tt="http://www.onvif.org/ver10/schema">%s</tt:Encoding>`+
		`<tt:Resolution xmlns:tt="http://www.onvif.org/ver10/schema"><tt:Width>%d</tt:Width><tt:Height>%d</tt:Height></tt:Resolution>`+
		`<tt:Quality xmlns:tt="http://www.onvif.org/ver10/schema">%f</tt:Quality>`+
		`<tt:RateControl xmlns:tt="http://www.onvif.org/ver10/schema">`+
		`<tt:FrameRateLimit>%d</tt:FrameRateLimit>`+
		`<tt:EncodingInterval>%d</tt:EncodingInterval>`+
		`<tt:BitrateLimit>%d</tt:BitrateLimit>`+
		`</tt:RateControl>`+
		`</trt:Configuration>`+
		`<trt:ForcePersistence>true</trt:ForcePersistence>`,
		cfg.Token, cfg.Name, cfg.UseCount, cfg.Encoding,
		cfg.Resolution.Width, cfg.Resolution.Height, cfg.Quality,
		cfg.RateControl.FrameRateLimit, cfg.RateControl.EncodingInterval, cfg.RateControl.BitrateLimit)

	_, err := m.call(ctx, "SetVideoEncoderConfiguration", content, Mutating)
	return err
}

func decodeMediaOperation(data []byte, name string, target any) error {
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
		if !ok || start.Name.Space != mediaNamespace || start.Name.Local != name {
			continue
		}
		return decoder.DecodeElement(target, &start)
	}
}

func (m *MediaClient) call(ctx context.Context, action, content string, op Operation) ([]byte, error) {
	body := `<trt:` + action + ` xmlns:trt="` + mediaNamespace + `">` + content + `</trt:` + action + `>`
	return m.client.Call(ctx, m.endpoint, mediaNamespace+"/"+action, body, op)
}
