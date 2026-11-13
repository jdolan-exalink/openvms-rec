package onvif

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
)

const imagingNamespace = "http://www.onvif.org/ver20/imaging/wsdl"

// ImagingClient provides ONVIF Imaging service operations.
type ImagingClient struct {
	client   *Client
	endpoint Endpoint
}

func NewImagingClient(client *Client, endpoint Endpoint) *ImagingClient {
	return &ImagingClient{client: client, endpoint: endpoint}
}

type ImagingSettings struct {
	Brightness       float64           `xml:"http://www.onvif.org/ver10/schema Brightness" json:"brightness"`
	ColorSaturation  float64           `xml:"http://www.onvif.org/ver10/schema ColorSaturation" json:"color_saturation"`
	Contrast         float64           `xml:"http://www.onvif.org/ver10/schema Contrast" json:"contrast"`
	Sharpness        float64           `xml:"http://www.onvif.org/ver10/schema Sharpness" json:"sharpness"`
	IrCutFilter      string            `xml:"http://www.onvif.org/ver10/schema IrCutFilter" json:"ir_cut_filter"` // ON, OFF, AUTO
	WideDynamicRange *WideDynamicRange `xml:"http://www.onvif.org/ver10/schema WideDynamicRange" json:"wide_dynamic_range,omitempty"`
}

// ImagingSettingsUpdate contains only controls explicitly requested by the API caller.
type ImagingSettingsUpdate struct {
	Brightness       *float64                `json:"brightness,omitempty"`
	ColorSaturation  *float64                `json:"color_saturation,omitempty"`
	Contrast         *float64                `json:"contrast,omitempty"`
	Sharpness        *float64                `json:"sharpness,omitempty"`
	IrCutFilter      *string                 `json:"ir_cut_filter,omitempty"`
	WideDynamicRange *WideDynamicRangeUpdate `json:"wide_dynamic_range,omitempty"`
}

type WideDynamicRangeUpdate struct {
	Mode  *string  `json:"mode,omitempty"`
	Level *float64 `json:"level,omitempty"`
}

type WideDynamicRange struct {
	Mode  string  `xml:"http://www.onvif.org/ver10/schema Mode" json:"mode"` // ON, OFF
	Level float64 `xml:"http://www.onvif.org/ver10/schema Level" json:"level"`
}

// FloatRange is an imaging setting range advertised by the device.
type ImagingFloatRange struct {
	Min    float64 `xml:"http://www.onvif.org/ver10/schema Min" json:"min"`
	Max    float64 `xml:"http://www.onvif.org/ver10/schema Max" json:"max"`
	minSet bool
	maxSet bool
}

func (r *ImagingFloatRange) UnmarshalXML(decoder *xml.Decoder, start xml.StartElement) error {
	var wire struct {
		Min *float64 `xml:"http://www.onvif.org/ver10/schema Min"`
		Max *float64 `xml:"http://www.onvif.org/ver10/schema Max"`
	}
	if err := decoder.DecodeElement(&wire, &start); err != nil {
		return err
	}
	if wire.Min != nil {
		r.Min, r.minSet = *wire.Min, true
	}
	if wire.Max != nil {
		r.Max, r.maxSet = *wire.Max, true
	}
	return nil
}

func (r ImagingFloatRange) valid() bool { return r.minSet && r.maxSet && r.Min <= r.Max }

// WideDynamicRangeOptions contains only options explicitly advertised by the device.
type WideDynamicRangeOptions struct {
	Modes []string           `xml:"http://www.onvif.org/ver10/schema Mode" json:"modes,omitempty"`
	Level *ImagingFloatRange `xml:"http://www.onvif.org/ver10/schema Level" json:"level,omitempty"`
}

// ImagingOptions describes supported imaging controls. Nil fields mean the device
// did not advertise that control; callers should leave it disabled.
type ImagingOptions struct {
	Brightness       *ImagingFloatRange       `xml:"http://www.onvif.org/ver10/schema Brightness" json:"brightness,omitempty"`
	ColorSaturation  *ImagingFloatRange       `xml:"http://www.onvif.org/ver10/schema ColorSaturation" json:"color_saturation,omitempty"`
	Contrast         *ImagingFloatRange       `xml:"http://www.onvif.org/ver10/schema Contrast" json:"contrast,omitempty"`
	Sharpness        *ImagingFloatRange       `xml:"http://www.onvif.org/ver10/schema Sharpness" json:"sharpness,omitempty"`
	IrCutFilterModes []string                 `xml:"http://www.onvif.org/ver10/schema IrCutFilterModes" json:"ir_cut_filter_modes,omitempty"`
	WideDynamicRange *WideDynamicRangeOptions `xml:"http://www.onvif.org/ver10/schema WideDynamicRange" json:"wide_dynamic_range,omitempty"`
}

func (i *ImagingClient) GetImagingSettings(ctx context.Context, videoSourceToken string) (ImagingSettings, error) {
	content := `<timg:VideoSourceToken>` + videoSourceToken + `</timg:VideoSourceToken>`
	data, err := i.call(ctx, "GetImagingSettings", content, ReadOnly)
	if err != nil {
		return ImagingSettings{}, err
	}
	var response struct {
		Settings ImagingSettings `xml:"http://www.onvif.org/ver20/imaging/wsdl ImagingSettings"`
	}
	if err := decodeImagingOperation(data, "GetImagingSettingsResponse", &response); err != nil {
		return ImagingSettings{}, &CallError{Status: StatusFailed, Code: "invalid_imaging_settings"}
	}
	return response.Settings, nil
}

func (i *ImagingClient) GetImagingOptions(ctx context.Context, videoSourceToken string) (ImagingOptions, error) {
	content := `<timg:VideoSourceToken>` + videoSourceToken + `</timg:VideoSourceToken>`
	data, err := i.call(ctx, "GetOptions", content, ReadOnly)
	if err != nil {
		return ImagingOptions{}, err
	}
	var response struct {
		Options ImagingOptions `xml:"http://www.onvif.org/ver20/imaging/wsdl ImagingOptions"`
	}
	if err := decodeImagingOperation(data, "GetOptionsResponse", &response); err != nil {
		return ImagingOptions{}, &CallError{Status: StatusFailed, Code: "invalid_imaging_options"}
	}
	response.Options.Brightness = validImagingRange(response.Options.Brightness)
	response.Options.ColorSaturation = validImagingRange(response.Options.ColorSaturation)
	response.Options.Contrast = validImagingRange(response.Options.Contrast)
	response.Options.Sharpness = validImagingRange(response.Options.Sharpness)
	if response.Options.WideDynamicRange != nil {
		response.Options.WideDynamicRange.Level = validImagingRange(response.Options.WideDynamicRange.Level)
		if len(response.Options.WideDynamicRange.Modes) == 0 && response.Options.WideDynamicRange.Level == nil {
			response.Options.WideDynamicRange = nil
		}
	}
	return response.Options, nil
}

func validImagingRange(r *ImagingFloatRange) *ImagingFloatRange {
	if r == nil || !r.valid() {
		return nil
	}
	return r
}

func (i *ImagingClient) SetImagingSettings(ctx context.Context, videoSourceToken string, s ImagingSettingsUpdate) error {
	options, err := i.GetImagingOptions(ctx, videoSourceToken)
	if err != nil {
		return err
	}
	fields := make([]string, 0, 6)
	if s.Brightness != nil {
		if options.Brightness == nil {
			return &CallError{Status: StatusFailed, Code: "unsupported_brightness"}
		}
		if !options.Brightness.contains(*s.Brightness) {
			return fmt.Errorf("brightness outside advertised range")
		}
		fields = append(fields, fmt.Sprintf(`<tt:Brightness xmlns:tt="http://www.onvif.org/ver10/schema">%f</tt:Brightness>`, *s.Brightness))
	}
	if s.ColorSaturation != nil {
		if options.ColorSaturation == nil {
			return &CallError{Status: StatusFailed, Code: "unsupported_color_saturation"}
		}
		if !options.ColorSaturation.contains(*s.ColorSaturation) {
			return fmt.Errorf("color saturation outside advertised range")
		}
		fields = append(fields, fmt.Sprintf(`<tt:ColorSaturation xmlns:tt="http://www.onvif.org/ver10/schema">%f</tt:ColorSaturation>`, *s.ColorSaturation))
	}
	if s.Contrast != nil {
		if options.Contrast == nil {
			return &CallError{Status: StatusFailed, Code: "unsupported_contrast"}
		}
		if !options.Contrast.contains(*s.Contrast) {
			return fmt.Errorf("contrast outside advertised range")
		}
		fields = append(fields, fmt.Sprintf(`<tt:Contrast xmlns:tt="http://www.onvif.org/ver10/schema">%f</tt:Contrast>`, *s.Contrast))
	}
	if s.Sharpness != nil {
		if options.Sharpness == nil {
			return &CallError{Status: StatusFailed, Code: "unsupported_sharpness"}
		}
		if !options.Sharpness.contains(*s.Sharpness) {
			return fmt.Errorf("sharpness outside advertised range")
		}
		fields = append(fields, fmt.Sprintf(`<tt:Sharpness xmlns:tt="http://www.onvif.org/ver10/schema">%f</tt:Sharpness>`, *s.Sharpness))
	}
	if s.IrCutFilter != nil {
		if len(options.IrCutFilterModes) == 0 {
			return &CallError{Status: StatusFailed, Code: "unsupported_ir_cut_filter"}
		}
		if !containsOption(options.IrCutFilterModes, *s.IrCutFilter) {
			return fmt.Errorf("IR cut filter mode is not advertised")
		}
		fields = append(fields, fmt.Sprintf(`<tt:IrCutFilter xmlns:tt="http://www.onvif.org/ver10/schema">%s</tt:IrCutFilter>`, *s.IrCutFilter))
	}
	if s.WideDynamicRange != nil {
		if options.WideDynamicRange == nil {
			return &CallError{Status: StatusFailed, Code: "unsupported_wide_dynamic_range"}
		}
		wdrFields := ""
		if s.WideDynamicRange.Mode != nil {
			if len(options.WideDynamicRange.Modes) == 0 {
				return &CallError{Status: StatusFailed, Code: "unsupported_wdr_mode"}
			}
			if !containsOption(options.WideDynamicRange.Modes, *s.WideDynamicRange.Mode) {
				return fmt.Errorf("WDR mode is not advertised")
			}
			wdrFields += fmt.Sprintf(`<tt:Mode>%s</tt:Mode>`, *s.WideDynamicRange.Mode)
		}
		if s.WideDynamicRange.Level != nil {
			if options.WideDynamicRange.Level == nil {
				return &CallError{Status: StatusFailed, Code: "unsupported_wdr_level"}
			}
			if !options.WideDynamicRange.Level.contains(*s.WideDynamicRange.Level) {
				return fmt.Errorf("WDR level outside advertised range")
			}
			wdrFields += fmt.Sprintf(`<tt:Level>%f</tt:Level>`, *s.WideDynamicRange.Level)
		}
		if wdrFields != "" {
			fields = append(fields, `<tt:WideDynamicRange xmlns:tt="http://www.onvif.org/ver10/schema">`+wdrFields+`</tt:WideDynamicRange>`)
		}
	}
	if len(fields) == 0 {
		return &CallError{Status: StatusFailed, Code: "imaging_settings_unsupported"}
	}
	content := fmt.Sprintf(`<timg:VideoSourceToken>%s</timg:VideoSourceToken>`+
		`<timg:ImagingSettings>`+strings.Join(fields, ``)+
		`</timg:ImagingSettings>`+
		`<timg:ForcePersistence>true</timg:ForcePersistence>`,
		videoSourceToken)

	if _, err := i.call(ctx, "SetImagingSettings", content, Mutating); err != nil {
		return err
	}
	actual, err := i.GetImagingSettings(ctx, videoSourceToken)
	if err != nil {
		return fmt.Errorf("verify imaging settings: %w", err)
	}
	if !imagingSettingsMatch(s, actual) {
		return &CallError{Status: StatusFailed, Code: "imaging_settings_readback_mismatch"}
	}
	return nil
}

func (r ImagingFloatRange) contains(value float64) bool { return value >= r.Min && value <= r.Max }

func containsOption(options []string, value string) bool {
	for _, option := range options {
		if strings.EqualFold(option, value) {
			return true
		}
	}
	return false
}

func imagingSettingsMatch(requested ImagingSettingsUpdate, actual ImagingSettings) bool {
	matchFloat := func(want, got float64) bool { return math.Abs(want-got) <= 1e-5 }
	if requested.Brightness != nil && !matchFloat(*requested.Brightness, actual.Brightness) {
		return false
	}
	if requested.ColorSaturation != nil && !matchFloat(*requested.ColorSaturation, actual.ColorSaturation) {
		return false
	}
	if requested.Contrast != nil && !matchFloat(*requested.Contrast, actual.Contrast) {
		return false
	}
	if requested.Sharpness != nil && !matchFloat(*requested.Sharpness, actual.Sharpness) {
		return false
	}
	if requested.IrCutFilter != nil && !strings.EqualFold(*requested.IrCutFilter, actual.IrCutFilter) {
		return false
	}
	if requested.WideDynamicRange != nil {
		if actual.WideDynamicRange == nil {
			return false
		}
		if requested.WideDynamicRange.Mode != nil && !strings.EqualFold(*requested.WideDynamicRange.Mode, actual.WideDynamicRange.Mode) {
			return false
		}
		if requested.WideDynamicRange.Level != nil && !matchFloat(*requested.WideDynamicRange.Level, actual.WideDynamicRange.Level) {
			return false
		}
	}
	return true
}

func decodeImagingOperation(data []byte, name string, target any) error {
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
		if !ok || start.Name.Space != imagingNamespace || start.Name.Local != name {
			continue
		}
		return decoder.DecodeElement(target, &start)
	}
}

func (i *ImagingClient) call(ctx context.Context, action, content string, op Operation) ([]byte, error) {
	body := `<timg:` + action + ` xmlns:timg="` + imagingNamespace + `">` + content + `</timg:` + action + `>`
	return i.client.Call(ctx, i.endpoint, imagingNamespace+"/"+action, body, op)
}
