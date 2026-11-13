package onvif

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

const (
	deviceNamespace = "http://www.onvif.org/ver10/device/wsdl"
	schemaNamespace = "http://www.onvif.org/ver10/schema"
)

// DeviceClient provides read-only ONVIF Device service operations.
type DeviceClient struct {
	client   *Client
	endpoint Endpoint
}

func NewDeviceClient(client *Client, endpoint Endpoint) *DeviceClient {
	return &DeviceClient{client: client, endpoint: endpoint}
}

type DeviceInformation struct {
	Manufacturer    string `xml:"http://www.onvif.org/ver10/device/wsdl Manufacturer"`
	Model           string `xml:"http://www.onvif.org/ver10/device/wsdl Model"`
	FirmwareVersion string `xml:"http://www.onvif.org/ver10/device/wsdl FirmwareVersion"`
	SerialNumber    string `xml:"http://www.onvif.org/ver10/device/wsdl SerialNumber"`
	HardwareID      string `xml:"http://www.onvif.org/ver10/device/wsdl HardwareId"`
}

type ServiceVersion struct {
	Major int `xml:"http://www.onvif.org/ver10/schema Major"`
	Minor int `xml:"http://www.onvif.org/ver10/schema Minor"`
}

// Service endpoint strings are untrusted device-provided data. Callers must
// validate them before making a follow-up request.
type Service struct {
	Namespace string         `xml:"http://www.onvif.org/ver10/device/wsdl Namespace"`
	Endpoint  string         `xml:"http://www.onvif.org/ver10/device/wsdl XAddr"`
	Version   ServiceVersion `xml:"http://www.onvif.org/ver10/device/wsdl Version"`
}

func (s Service) ValidatedEndpoint() (Endpoint, error) {
	ep, err := ParseEndpoint(s.Endpoint)
	if err != nil {
		return Endpoint{}, fmt.Errorf("invalid ONVIF service endpoint: %w", ErrInvalidEndpoint)
	}
	return ep, nil
}

type DeviceClock struct {
	DateTimeType string        `xml:"http://www.onvif.org/ver10/schema DateTimeType"`
	UTC          DateTimeValue `xml:"http://www.onvif.org/ver10/schema UTCDateTime"`
	Local        DateTimeValue `xml:"http://www.onvif.org/ver10/schema LocalDateTime"`
}

type DateTimeValue struct {
	Time CalendarTime `xml:"http://www.onvif.org/ver10/schema Time"`
	Date CalendarDate `xml:"http://www.onvif.org/ver10/schema Date"`
}

type CalendarTime struct {
	Hour   int `xml:"http://www.onvif.org/ver10/schema Hour"`
	Minute int `xml:"http://www.onvif.org/ver10/schema Minute"`
	Second int `xml:"http://www.onvif.org/ver10/schema Second"`
}

type CalendarDate struct {
	Year  int `xml:"http://www.onvif.org/ver10/schema Year"`
	Month int `xml:"http://www.onvif.org/ver10/schema Month"`
	Day   int `xml:"http://www.onvif.org/ver10/schema Day"`
}

func (d *DeviceClient) GetDeviceInformation(ctx context.Context) (DeviceInformation, error) {
	data, err := d.call(ctx, "GetDeviceInformation", "")
	if err != nil {
		return DeviceInformation{}, err
	}
	var info DeviceInformation
	if err := decodeOperation(data, "GetDeviceInformationResponse", &info); err != nil {
		return DeviceInformation{}, &CallError{Status: StatusFailed, Code: "invalid_device_information"}
	}
	return info, nil
}

func (d *DeviceClient) GetServices(ctx context.Context) ([]Service, error) {
	data, err := d.call(ctx, "GetServices", `<tds:IncludeCapability>false</tds:IncludeCapability>`)
	if err != nil {
		return nil, err
	}
	var response struct {
		Services []Service `xml:"http://www.onvif.org/ver10/device/wsdl Service"`
	}
	if err := decodeOperation(data, "GetServicesResponse", &response); err != nil {
		return nil, &CallError{Status: StatusFailed, Code: "invalid_services"}
	}
	return response.Services, nil
}

type Capabilities struct {
	Device    *CapabilityEndpoint `xml:"Device"`
	Events    *CapabilityEndpoint `xml:"Events"`
	Imaging   *CapabilityEndpoint `xml:"Imaging"`
	Media     *CapabilityEndpoint `xml:"Media"`
	PTZ       *CapabilityEndpoint `xml:"PTZ"`
	Analytics *CapabilityEndpoint `xml:"Analytics"`
}

type CapabilityEndpoint struct {
	XAddr string `xml:"XAddr"`
}

func (d *DeviceClient) GetCapabilities(ctx context.Context) ([]Service, error) {
	data, err := d.call(ctx, "GetCapabilities", `<tds:Category>All</tds:Category>`)
	if err != nil {
		return nil, err
	}
	var response struct {
		Capabilities Capabilities `xml:"Capabilities"`
	}
	if err := decodeOperation(data, "GetCapabilitiesResponse", &response); err != nil {
		return nil, &CallError{Status: StatusFailed, Code: "invalid_capabilities"}
	}
	caps := response.Capabilities
	var services []Service
	mapping := []struct {
		endpoint *CapabilityEndpoint
		ns       string
	}{
		{caps.Media, "http://www.onvif.org/ver10/media/wsdl"},
		{caps.Device, "http://www.onvif.org/ver10/device/wsdl"},
		{caps.Events, "http://www.onvif.org/ver10/events/wsdl"},
		{caps.Imaging, "http://www.onvif.org/ver10/imaging/wsdl"},
		{caps.PTZ, "http://www.onvif.org/ver10/ptz/wsdl"},
		{caps.Analytics, "http://www.onvif.org/ver10/analytics/wsdl"},
	}
	for _, m := range mapping {
		if m.endpoint != nil && strings.TrimSpace(m.endpoint.XAddr) != "" {
			services = append(services, Service{
				Namespace: m.ns,
				Endpoint:  strings.TrimSpace(m.endpoint.XAddr),
				Version:   ServiceVersion{Major: 1, Minor: 0},
			})
		}
	}
	return services, nil
}

func (d *DeviceClient) GetSystemDateAndTime(ctx context.Context) (DeviceClock, error) {
	data, err := d.call(ctx, "GetSystemDateAndTime", "")
	if err != nil {
		return DeviceClock{}, err
	}
	var response struct {
		Clock DeviceClock `xml:"http://www.onvif.org/ver10/device/wsdl SystemDateAndTime"`
	}
	if err := decodeOperation(data, "GetSystemDateAndTimeResponse", &response); err != nil {
		return DeviceClock{}, &CallError{Status: StatusFailed, Code: "invalid_system_time"}
	}
	return response.Clock, nil
}

func (d *DeviceClient) SetSystemDateAndTime(ctx context.Context, utcTime time.Time) error {
	utc := utcTime.UTC()
	content := fmt.Sprintf(`<tds:DateTimeType>Manual</tds:DateTimeType>`+
		`<tds:DaylightSavings>false</tds:DaylightSavings>`+
		`<tds:TimeZone><tt:TZ xmlns:tt="http://www.onvif.org/ver10/schema">UTC</tt:TZ></tds:TimeZone>`+
		`<tds:UTCDateTime>`+
		`<tt:Time xmlns:tt="http://www.onvif.org/ver10/schema">`+
		`<tt:Hour>%d</tt:Hour>`+
		`<tt:Minute>%d</tt:Minute>`+
		`<tt:Second>%d</tt:Second>`+
		`</tt:Time>`+
		`<tt:Date xmlns:tt="http://www.onvif.org/ver10/schema">`+
		`<tt:Year>%d</tt:Year>`+
		`<tt:Month>%d</tt:Month>`+
		`<tt:Day>%d</tt:Day>`+
		`</tt:Date>`+
		`</tds:UTCDateTime>`,
		utc.Hour(), utc.Minute(), utc.Second(),
		utc.Year(), int(utc.Month()), utc.Day())

	body := `<tds:SetSystemDateAndTime xmlns:tds="` + deviceNamespace + `">` + content + `</tds:SetSystemDateAndTime>`
	_, err := d.client.Call(ctx, d.endpoint, deviceNamespace+"/SetSystemDateAndTime", body, Mutating)
	return err
}

// SystemReboot asks the device to restart. The camera stops streaming until it is back; the
// device may answer before it goes down or drop the connection, so the caller should not treat a
// transport failure after the request as proof that nothing happened.
func (d *DeviceClient) SystemReboot(ctx context.Context) error {
	body := `<tds:SystemReboot xmlns:tds="` + deviceNamespace + `"/>`
	_, err := d.client.Call(ctx, d.endpoint, deviceNamespace+"/SystemReboot", body, Mutating)
	return err
}

func decodeOperation(data []byte, name string, target any) error {
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
		if !ok || start.Name.Space != deviceNamespace || start.Name.Local != name {
			continue
		}
		return decoder.DecodeElement(target, &start)
	}
}

func (d *DeviceClient) call(ctx context.Context, action, content string) ([]byte, error) {
	body := `<tds:` + action + ` xmlns:tds="` + deviceNamespace + `">` + content + `</tds:` + action + `>`
	return d.client.Call(ctx, d.endpoint, deviceNamespace+"/"+action, body, ReadOnly)
}
