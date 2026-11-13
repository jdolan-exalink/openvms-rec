package onvif

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
)

const ptzNamespace = "http://www.onvif.org/ver20/ptz/wsdl"

// PTZClient provides ONVIF PTZ service operations.
type PTZClient struct {
	client   *Client
	endpoint Endpoint
}

func NewPTZClient(client *Client, endpoint Endpoint) *PTZClient {
	return &PTZClient{client: client, endpoint: endpoint}
}

type PTZPreset struct {
	Token string `xml:"token,attr" json:"token"`
	Name  string `xml:"http://www.onvif.org/ver10/schema Name" json:"name"`
}

func (p *PTZClient) ContinuousMove(ctx context.Context, profileToken string, panSpeed, tiltSpeed, zoomSpeed float64, timeoutSeconds int) error {
	if timeoutSeconds <= 0 {
		timeoutSeconds = 5
	}
	content := fmt.Sprintf(`<tptz:ProfileToken>%s</tptz:ProfileToken>`+
		`<tptz:Velocity>`+
		`<tt:PanTilt xmlns:tt="http://www.onvif.org/ver10/schema" x="%f" y="%f" space="http://www.onvif.org/ver10/tptz/PanTiltSpaces/VelocityGenericSpace"/>`+
		`<tt:Zoom xmlns:tt="http://www.onvif.org/ver10/schema" x="%f" space="http://www.onvif.org/ver10/tptz/ZoomSpaces/VelocityGenericSpace"/>`+
		`</tptz:Velocity>`+
		`<tptz:Timeout>PT%dS</tptz:Timeout>`,
		profileToken, panSpeed, tiltSpeed, zoomSpeed, timeoutSeconds)

	_, err := p.call(ctx, "ContinuousMove", content, Mutating)
	return err
}

func (p *PTZClient) Stop(ctx context.Context, profileToken string, panTilt, zoom bool) error {
	content := fmt.Sprintf(`<tptz:ProfileToken>%s</tptz:ProfileToken>`+
		`<tptz:PanTilt>%t</tptz:PanTilt>`+
		`<tptz:Zoom>%t</tptz:Zoom>`,
		profileToken, panTilt, zoom)

	_, err := p.call(ctx, "Stop", content, Mutating)
	return err
}

func (p *PTZClient) AbsoluteMove(ctx context.Context, profileToken string, x, y, zoom float64) error {
	content := fmt.Sprintf(`<tptz:ProfileToken>%s</tptz:ProfileToken>`+
		`<tptz:Position>`+
		`<tt:PanTilt xmlns:tt="http://www.onvif.org/ver10/schema" x="%f" y="%f" space="http://www.onvif.org/ver10/tptz/PanTiltSpaces/PositionGenericSpace"/>`+
		`<tt:Zoom xmlns:tt="http://www.onvif.org/ver10/schema" x="%f" space="http://www.onvif.org/ver10/tptz/ZoomSpaces/PositionGenericSpace"/>`+
		`</tptz:Position>`,
		profileToken, x, y, zoom)

	_, err := p.call(ctx, "AbsoluteMove", content, Mutating)
	return err
}

func (p *PTZClient) GetPresets(ctx context.Context, profileToken string) ([]PTZPreset, error) {
	content := `<tptz:ProfileToken>` + profileToken + `</tptz:ProfileToken>`
	data, err := p.call(ctx, "GetPresets", content, ReadOnly)
	if err != nil {
		return nil, err
	}
	var response struct {
		Presets []PTZPreset `xml:"http://www.onvif.org/ver20/ptz/wsdl Preset"`
	}
	if err := decodePTZOperation(data, "GetPresetsResponse", &response); err != nil {
		return nil, &CallError{Status: StatusFailed, Code: "invalid_ptz_presets"}
	}
	return response.Presets, nil
}

func (p *PTZClient) GotoPreset(ctx context.Context, profileToken, presetToken string) error {
	content := fmt.Sprintf(`<tptz:ProfileToken>%s</tptz:ProfileToken>`+
		`<tptz:PresetToken>%s</tptz:PresetToken>`,
		profileToken, presetToken)

	_, err := p.call(ctx, "GotoPreset", content, Mutating)
	return err
}

// SetPreset saves the current position as a preset. An empty presetToken creates a new preset;
// an existing token overwrites that preset (ONVIF has no rename: renaming is SetPreset with the
// same token and a new name, which also overwrites the stored position). It returns the token.
func (p *PTZClient) SetPreset(ctx context.Context, profileToken, name, presetToken string) (string, error) {
	content := `<tptz:ProfileToken>` + escapeXML(profileToken) + `</tptz:ProfileToken>`
	if name != "" {
		// The name is optional in ONVIF; without one the device picks its own.
		content += `<tptz:PresetName>` + escapeXML(name) + `</tptz:PresetName>`
	}
	if presetToken != "" {
		content += `<tptz:PresetToken>` + escapeXML(presetToken) + `</tptz:PresetToken>`
	}
	data, err := p.call(ctx, "SetPreset", content, Mutating)
	if err != nil {
		return "", err
	}
	var response struct {
		PresetToken string `xml:"http://www.onvif.org/ver20/ptz/wsdl PresetToken"`
	}
	if err := decodePTZOperation(data, "SetPresetResponse", &response); err != nil {
		return "", &CallError{Status: StatusFailed, Code: "invalid_ptz_preset"}
	}
	return response.PresetToken, nil
}

// RemovePreset deletes a preset.
func (p *PTZClient) RemovePreset(ctx context.Context, profileToken, presetToken string) error {
	content := `<tptz:ProfileToken>` + escapeXML(profileToken) + `</tptz:ProfileToken>` +
		`<tptz:PresetToken>` + escapeXML(presetToken) + `</tptz:PresetToken>`
	_, err := p.call(ctx, "RemovePreset", content, Mutating)
	return err
}

// GotoHomePosition moves the camera to its home position.
func (p *PTZClient) GotoHomePosition(ctx context.Context, profileToken string) error {
	_, err := p.call(ctx, "GotoHomePosition", `<tptz:ProfileToken>`+escapeXML(profileToken)+`</tptz:ProfileToken>`, Mutating)
	return err
}

// SetHomePosition stores the current position as the home position.
func (p *PTZClient) SetHomePosition(ctx context.Context, profileToken string) error {
	_, err := p.call(ctx, "SetHomePosition", `<tptz:ProfileToken>`+escapeXML(profileToken)+`</tptz:ProfileToken>`, Mutating)
	return err
}

// PTZNode describes what a PTZ node really supports; the UI enables only these operations.
type PTZNode struct {
	Token             string `json:"token"`
	Name              string `json:"name"`
	MaximumPresets    int    `json:"maximum_presets"`
	HomeSupported     bool   `json:"home_supported"`
	FixedHomePosition bool   `json:"fixed_home_position"`
	SupportsPanTilt   bool   `json:"supports_pan_tilt"`
	SupportsZoom      bool   `json:"supports_zoom"`
}

// GetNodes returns the PTZ nodes of the device with their capabilities.
func (p *PTZClient) GetNodes(ctx context.Context) ([]PTZNode, error) {
	data, err := p.call(ctx, "GetNodes", "", ReadOnly)
	if err != nil {
		return nil, err
	}
	var response struct {
		Nodes []struct {
			Token             string `xml:"token,attr"`
			FixedHomePosition string `xml:"FixedHomePosition,attr"`
			Name              string `xml:"http://www.onvif.org/ver10/schema Name"`
			HomeSupported     string `xml:"http://www.onvif.org/ver10/schema HomeSupported"`
			MaximumPresets    int    `xml:"http://www.onvif.org/ver10/schema MaximumNumberOfPresets"`
			Spaces            struct {
				PanTilt []struct{} `xml:"http://www.onvif.org/ver10/schema ContinuousPanTiltVelocitySpace"`
				Zoom    []struct{} `xml:"http://www.onvif.org/ver10/schema ContinuousZoomVelocitySpace"`
			} `xml:"http://www.onvif.org/ver10/schema SupportedPTZSpaces"`
		} `xml:"http://www.onvif.org/ver20/ptz/wsdl PTZNode"`
	}
	if err := decodePTZOperation(data, "GetNodesResponse", &response); err != nil {
		return nil, &CallError{Status: StatusFailed, Code: "invalid_ptz_nodes"}
	}
	out := make([]PTZNode, 0, len(response.Nodes))
	for _, n := range response.Nodes {
		out = append(out, PTZNode{
			Token: n.Token, Name: n.Name, MaximumPresets: n.MaximumPresets,
			HomeSupported:     n.HomeSupported == "true" || n.HomeSupported == "1",
			FixedHomePosition: n.FixedHomePosition == "true" || n.FixedHomePosition == "1",
			SupportsPanTilt:   len(n.Spaces.PanTilt) > 0,
			SupportsZoom:      len(n.Spaces.Zoom) > 0,
		})
	}
	return out, nil
}

func decodePTZOperation(data []byte, name string, target any) error {
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
		if !ok || start.Name.Space != ptzNamespace || start.Name.Local != name {
			continue
		}
		return decoder.DecodeElement(target, &start)
	}
}

func (p *PTZClient) call(ctx context.Context, action, content string, op Operation) ([]byte, error) {
	body := `<tptz:` + action + ` xmlns:tptz="` + ptzNamespace + `">` + content + `</tptz:` + action + `>`
	return p.client.Call(ctx, p.endpoint, ptzNamespace+"/"+action, body, op)
}
