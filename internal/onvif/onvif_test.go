package onvif

import (
	"context"
	"io"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func soapResp(status int, body string) *http.Response {
	env := `<?xml version="1.0" encoding="UTF-8"?><s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope"><s:Body>` + body + `</s:Body></s:Envelope>`
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/soap+xml"}},
		Body:       io.NopCloser(strings.NewReader(env)),
	}
}

func TestProbeFullAndMedia2Fallback(t *testing.T) {
	now := time.Date(2026, 11, 13, 15, 0, 0, 0, time.UTC)
	tr := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		reqXML := string(b)
		switch {
		case strings.Contains(reqXML, "GetSystemDateAndTime"):
			return soapResp(200, `<tds:GetSystemDateAndTimeResponse xmlns:tds="http://www.onvif.org/ver10/device/wsdl" xmlns:tt="http://www.onvif.org/ver10/schema">
				<tds:SystemDateAndTime><tt:DateTimeType>NTP</tt:DateTimeType>
				<tt:UTCDateTime><tt:Time><tt:Hour>15</tt:Hour><tt:Minute>0</tt:Minute><tt:Second>2</tt:Second></tt:Time>
				<tt:Date><tt:Year>2026</tt:Year><tt:Month>11</tt:Month><tt:Day>13</tt:Day></tt:Date></tt:UTCDateTime>
				</tds:SystemDateAndTime></tds:GetSystemDateAndTimeResponse>`), nil
		case strings.Contains(reqXML, "GetDeviceInformation"):
			if !strings.Contains(reqXML, "UsernameToken") {
				return soapResp(401, ""), nil
			}
			return soapResp(200, `<tds:GetDeviceInformationResponse xmlns:tds="http://www.onvif.org/ver10/device/wsdl">
				<tds:Manufacturer>Provision-ISR</tds:Manufacturer>
				<tds:Model>I4-320IPSN-VF-V4</tds:Model>
				<tds:FirmwareVersion>5.1.3</tds:FirmwareVersion>
				<tds:SerialNumber>SN12345</tds:SerialNumber>
				<tds:HardwareId>HW1</tds:HardwareId>
			</tds:GetDeviceInformationResponse>`), nil
		case strings.Contains(reqXML, "GetServices"):
			return soapResp(200, `<tds:GetServicesResponse xmlns:tds="http://www.onvif.org/ver10/device/wsdl">
				<tds:Service><tds:Namespace>http://www.onvif.org/ver20/media/wsdl</tds:Namespace><tds:XAddr>http://192.0.2.225/onvif/media2_service</tds:XAddr></tds:Service>
				<tds:Service><tds:Namespace>http://www.onvif.org/ver10/media/wsdl</tds:Namespace><tds:XAddr>http://192.0.2.225/onvif/media_service</tds:XAddr></tds:Service>
				<tds:Service><tds:Namespace>http://www.onvif.org/ver10/events/wsdl</tds:Namespace><tds:XAddr>http://192.0.2.225/onvif/events_service</tds:XAddr></tds:Service>
			</tds:GetServicesResponse>`), nil
		case strings.Contains(reqXML, "tr2:GetProfiles"):
			// Simulate Provision firmware returning SOAP fault on Media2 GetProfiles
			return soapResp(400, `<s:Fault><s:Code><s:Value>s:Sender</s:Value><s:Subcode><s:Value>ter:ActionNotSupported</s:Value></s:Subcode></s:Code><s:Reason><s:Text>Not supported</s:Text></s:Reason></s:Fault>`), nil
		case strings.Contains(reqXML, "trt:GetProfiles"):
			return soapResp(200, `<trt:GetProfilesResponse xmlns:trt="http://www.onvif.org/ver10/media/wsdl" xmlns:tt="http://www.onvif.org/ver10/schema">
				<trt:Profiles token="Profile_1"><tt:Name>mainStream</tt:Name>
					<tt:VideoEncoderConfiguration><tt:Encoding>H265</tt:Encoding><tt:Resolution><tt:Width>2560</tt:Width><tt:Height>1440</tt:Height></tt:Resolution><tt:RateControl><tt:FrameRateLimit>15</tt:FrameRateLimit><tt:BitrateLimit>2560</tt:BitrateLimit></tt:RateControl></tt:VideoEncoderConfiguration>
				</trt:Profiles>
				<trt:Profiles token="Profile_2"><tt:Name>subStream</tt:Name>
					<tt:VideoEncoderConfiguration><tt:Encoding>H264</tt:Encoding><tt:Resolution><tt:Width>704</tt:Width><tt:Height>576</tt:Height></tt:Resolution><tt:RateControl><tt:FrameRateLimit>15</tt:FrameRateLimit><tt:BitrateLimit>512</tt:BitrateLimit></tt:RateControl></tt:VideoEncoderConfiguration>
				</trt:Profiles>
			</trt:GetProfilesResponse>`), nil
		case strings.Contains(reqXML, "GetStreamUri"):
			if strings.Contains(reqXML, "Profile_1") {
				return soapResp(200, `<trt:GetStreamUriResponse xmlns:trt="http://www.onvif.org/ver10/media/wsdl" xmlns:tt="http://www.onvif.org/ver10/schema">
					<trt:MediaUri><tt:Uri>rtsp://admin:secret@192.0.2.225:554/profile1</tt:Uri></trt:MediaUri></trt:GetStreamUriResponse>`), nil
			}
			return soapResp(200, `<trt:GetStreamUriResponse xmlns:trt="http://www.onvif.org/ver10/media/wsdl" xmlns:tt="http://www.onvif.org/ver10/schema">
				<trt:MediaUri><tt:Uri>rtsp://192.0.2.225:554/profile2</tt:Uri></trt:MediaUri></trt:GetStreamUriResponse>`), nil
		case strings.Contains(reqXML, "GetEventProperties"):
			return soapResp(200, `<tev:GetEventPropertiesResponse xmlns:tev="http://www.onvif.org/ver10/events/wsdl" xmlns:wstop="http://docs.oasis-open.org/wsn/t-1">
				<wstop:TopicSet>
					<RuleEngine><CellMotionDetector><Motion wstop:topic="true"/></CellMotionDetector>
					<LineDetector><Crossed wstop:topic="true"/></LineDetector></RuleEngine>
					<VideoAnalytics><LicensePlate wstop:topic="true"/></VideoAnalytics>
				</wstop:TopicSet></tev:GetEventPropertiesResponse>`), nil
		case strings.Contains(reqXML, "CreatePullPointSubscription"):
			return soapResp(200, `<tev:CreatePullPointSubscriptionResponse xmlns:tev="http://www.onvif.org/ver10/events/wsdl">
				<tev:SubscriptionReference><Address>http://192.0.2.225/onvif/sub1</Address></tev:SubscriptionReference>
			</tev:CreatePullPointSubscriptionResponse>`), nil
		}
		return soapResp(404, ""), nil
	})

	res, err := Probe(context.Background(), tr, ProbeRequest{
		Host:      "192.0.2.225",
		ONVIFPort: 80,
		Username:  "admin",
		Password:  "secret",
	}, now)
	if err != nil {
		t.Fatalf("Probe failed: %v", err)
	}
	if res.Manufacturer != "Provision-ISR" || res.Model != "I4-320IPSN-VF-V4" {
		t.Fatalf("unexpected device info: %+v", res)
	}
	if res.ClockOffsetMS != 2000 {
		t.Fatalf("expected 2000ms clock offset, got %d", res.ClockOffsetMS)
	}
	// Credentials must be stripped from MainURL
	if res.MainURL != "rtsp://192.0.2.225:554/profile1" {
		t.Fatalf("expected sanitized main_url, got %q", res.MainURL)
	}
	if res.SubURL != "rtsp://192.0.2.225:554/profile2" {
		t.Fatalf("expected sub_url, got %q", res.SubURL)
	}

	byCap := map[string]CapabilityStatus{}
	for _, c := range res.Capabilities {
		byCap[c.Capability] = c.Status
		if strings.Contains(c.Evidence, "secret") {
			t.Fatalf("secret leaked in evidence: %s", c.Evidence)
		}
	}
	if byCap["onvif.device.info"] != CapConfirmed {
		t.Errorf("device.info = %v, want confirmed", byCap["onvif.device.info"])
	}
	if byCap["onvif.media2.profiles"] != CapUnsupported {
		t.Errorf("media2.profiles = %v, want unsupported", byCap["onvif.media2.profiles"])
	}
	if byCap["onvif.media.profiles"] != CapConfirmed {
		t.Errorf("media.profiles = %v, want confirmed", byCap["onvif.media.profiles"])
	}
	if byCap["onvif.events.pullpoint"] != CapConfirmed {
		t.Errorf("events.pullpoint = %v, want confirmed", byCap["onvif.events.pullpoint"])
	}
	// Rule 4: advertised topics are reported as "unknown" until a real event arrives
	if byCap["analytics.motion"] != CapUnknown || byCap["analytics.line_crossing"] != CapUnknown || byCap["analytics.lpr"] != CapUnknown {
		t.Errorf("expected advertised analytics to be unknown until real event, got %+v", byCap)
	}
}

func TestClassifyNotification(t *testing.T) {
	c1 := ClassifyNotification(EventNotification{
		Topic: "tns1:RuleEngine/CellMotionDetector/Motion",
		Data:  map[string]string{"IsMotion": "true", "VideoSourceConfigurationToken": "src1"},
	})
	if !c1.Active || c1.Type != "motion" {
		t.Fatalf("expected active motion, got %+v", c1)
	}

	c2 := ClassifyNotification(EventNotification{
		Topic: "tns1:RuleEngine/CellMotionDetector/Motion",
		Data:  map[string]string{"IsMotion": "false"},
	})
	if c2.Active {
		t.Fatalf("expected inactive motion when IsMotion=false, got %+v", c2)
	}

	c3 := ClassifyNotification(EventNotification{
		Topic: "tns1:RuleEngine/LineDetector/Crossed",
		Data:  map[string]string{"ObjectId": "42", "RuleName": "Linea1"},
	})
	if !c3.Active || c3.Type != "line_crossing" || c3.Zone != "Linea1" {
		t.Fatalf("expected active line_crossing, got %+v", c3)
	}
}

func TestParseProbeMatches(t *testing.T) {
	pkt := []byte(`<?xml version="1.0" encoding="UTF-8"?>
	<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope" xmlns:a="http://schemas.xmlsoap.org/ws/2004/08/addressing" xmlns:d="http://schemas.xmlsoap.org/ws/2005/04/discovery">
		<s:Header>
			<a:Action>http://schemas.xmlsoap.org/ws/2005/04/discovery/ProbeMatches</a:Action>
			<a:RelatesTo>urn:uuid:1111</a:RelatesTo>
		</s:Header>
		<s:Body>
			<d:ProbeMatches>
				<d:ProbeMatch>
					<a:EndpointReference><a:Address>urn:uuid:cam-225</a:Address></a:EndpointReference>
					<d:Scopes>onvif://www.onvif.org/name/IPC-Cochera onvif://www.onvif.org/hardware/I4-320IPSN</d:Scopes>
					<d:XAddrs>http://192.0.2.225:80/onvif/device_service</d:XAddrs>
				</d:ProbeMatch>
			</d:ProbeMatches>
		</s:Body>
	</s:Envelope>`)
	devs := ParseProbeMatches(pkt, "urn:uuid:1111")
	if len(devs) != 1 {
		t.Fatalf("expected 1 device, got %d", len(devs))
	}
	if devs[0].Host != "192.0.2.225" || devs[0].ONVIFPort != 80 || devs[0].Name != "IPC-Cochera" || devs[0].Hardware != "I4-320IPSN" {
		t.Fatalf("unexpected device: %+v", devs[0])
	}
}

func TestPullMessagesVideoAnalyticsAndProfileG(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	tr := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		req := string(b)
		switch {
		case strings.Contains(req, "GetSystemDateAndTime"):
			return soapResp(200, `<tds:GetSystemDateAndTimeResponse xmlns:tds="http://www.onvif.org/ver10/device/wsdl" xmlns:tt="http://www.onvif.org/ver10/schema"><tds:SystemDateAndTime><tt:UTCDateTime><tt:Time><tt:Hour>12</tt:Hour><tt:Minute>0</tt:Minute><tt:Second>0</tt:Second></tt:Time><tt:Date><tt:Year>2026</tt:Year><tt:Month>11</tt:Month><tt:Day>16</tt:Day></tt:Date></tt:UTCDateTime></tds:SystemDateAndTime></tds:GetSystemDateAndTimeResponse>`), nil
		case strings.Contains(req, "GetDeviceInformation"):
			return soapResp(200, `<tds:GetDeviceInformationResponse xmlns:tds="http://www.onvif.org/ver10/device/wsdl"><tds:Manufacturer>Provision-ISR</tds:Manufacturer><tds:Model>DI-340IPSN</tds:Model><tds:FirmwareVersion>5.1</tds:FirmwareVersion></tds:GetDeviceInformationResponse>`), nil
		case strings.Contains(req, "GetServices"):
			return soapResp(200, `<tds:GetServicesResponse xmlns:tds="http://www.onvif.org/ver10/device/wsdl">
				<tds:Service><tds:Namespace>http://www.onvif.org/ver20/media/wsdl</tds:Namespace><tds:XAddr>http://192.0.2.225/onvif/media2</tds:XAddr></tds:Service>
				<tds:Service><tds:Namespace>http://www.onvif.org/ver10/events/wsdl</tds:Namespace><tds:XAddr>http://192.0.2.225/onvif/events</tds:XAddr></tds:Service>
				<tds:Service><tds:Namespace>http://www.onvif.org/ver10/search/wsdl</tds:Namespace><tds:XAddr>http://192.0.2.225/onvif/search</tds:XAddr></tds:Service>
				<tds:Service><tds:Namespace>http://www.onvif.org/ver10/replay/wsdl</tds:Namespace><tds:XAddr>http://192.0.2.225/onvif/replay</tds:XAddr></tds:Service>
			</tds:GetServicesResponse>`), nil
		case strings.Contains(req, "GetProfiles"):
			return soapResp(200, `<tr2:GetProfilesResponse xmlns:tr2="http://www.onvif.org/ver20/media/wsdl" xmlns:tt="http://www.onvif.org/ver10/schema"><tr2:Profiles token="p1"><tr2:Configurations><tr2:VideoEncoder Encoding="H264"><tr2:Resolution Width="1920" Height="1080"/></tr2:VideoEncoder></tr2:Configurations></tr2:Profiles></tr2:GetProfilesResponse>`), nil
		case strings.Contains(req, "GetStreamUri"):
			return soapResp(200, `<tr2:GetStreamUriResponse xmlns:tr2="http://www.onvif.org/ver20/media/wsdl"><tr2:Uri>rtsp://192.0.2.225:554/profile1</tr2:Uri></tr2:GetStreamUriResponse>`), nil
		case strings.Contains(req, "GetRecordingSummary"):
			return soapResp(200, `<tse:GetRecordingSummaryResponse xmlns:tse="http://www.onvif.org/ver10/search/wsdl"><tse:Summary><tt:DataFrom xmlns:tt="http://www.onvif.org/ver10/schema">2026-11-16T08:00:00Z</tt:DataFrom><tt:DataUntil xmlns:tt="http://www.onvif.org/ver10/schema">2026-11-16T12:00:00Z</tt:DataUntil><tt:NumberRecordings>1</tt:NumberRecordings></tse:Summary></tse:GetRecordingSummaryResponse>`), nil
		case strings.Contains(req, "FindRecordings"):
			return soapResp(200, `<tse:FindRecordingsResponse xmlns:tse="http://www.onvif.org/ver10/search/wsdl"><tse:SearchToken>srch_1</tse:SearchToken></tse:FindRecordingsResponse>`), nil
		case strings.Contains(req, "GetRecordingSearchResults"):
			return soapResp(200, `<tse:GetRecordingSearchResultsResponse xmlns:tse="http://www.onvif.org/ver10/search/wsdl" xmlns:tt="http://www.onvif.org/ver10/schema"><tse:ResultList><tt:SearchState>Completed</tt:SearchState><tt:RecordingInformation><tt:RecordingToken>rec_sd_1</tt:RecordingToken><tt:EarliestRecording>2026-11-16T08:00:00Z</tt:EarliestRecording><tt:LatestRecording>2026-11-16T12:00:00Z</tt:LatestRecording><tt:Track><tt:TrackType>Video</tt:TrackType></tt:Track></tt:RecordingInformation></tse:ResultList></tse:GetRecordingSearchResultsResponse>`), nil
		case strings.Contains(req, "EndSearch"):
			return soapResp(200, `<tse:EndSearchResponse xmlns:tse="http://www.onvif.org/ver10/search/wsdl"/>`), nil
		case strings.Contains(req, "GetReplayUri"):
			return soapResp(200, `<trp:GetReplayUriResponse xmlns:trp="http://www.onvif.org/ver10/replay/wsdl"><trp:Uri>rtsp://admin:secret@192.0.2.225:554/replay/rec_sd_1</trp:Uri></trp:GetReplayUriResponse>`), nil
		case strings.Contains(req, "GetEventProperties"):
			return soapResp(200, `<tev:GetEventPropertiesResponse xmlns:tev="http://www.onvif.org/ver10/events/wsdl" xmlns:wstop="http://docs.oasis-open.org/wsn/t-1" xmlns:tns1="http://www.onvif.org/ver10/topics"><wstop:TopicSet><tns1:RuleEngine><FieldDetector><ObjectsInside wstop:topic="true"/></FieldDetector></tns1:RuleEngine></wstop:TopicSet></tev:GetEventPropertiesResponse>`), nil
		case strings.Contains(req, "CreatePullPointSubscription"):
			return soapResp(200, `<tev:CreatePullPointSubscriptionResponse xmlns:tev="http://www.onvif.org/ver10/events/wsdl"><tev:SubscriptionReference><a:Address xmlns:a="http://www.w3.org/2005/08/addressing">http://192.0.2.225/sub1</a:Address></tev:SubscriptionReference></tev:CreatePullPointSubscriptionResponse>`), nil
		case strings.Contains(req, "PullMessages"):
			return soapResp(200, `<tev:PullMessagesResponse xmlns:tev="http://www.onvif.org/ver10/events/wsdl" xmlns:wsnt="http://docs.oasis-open.org/wsn/b-2" xmlns:tt="http://www.onvif.org/ver10/schema">
				<wsnt:NotificationMessage>
					<wsnt:Topic>tns1:RuleEngine/FieldDetector/ObjectsInside</wsnt:Topic>
					<wsnt:Message><tt:Message UtcTime="2026-11-16T12:00:01Z" PropertyOperation="Changed">
						<tt:Source><tt:SimpleItem Name="Rule" Value="CocheraZona1"/></tt:Source>
						<tt:Data>
							<tt:SimpleItem Name="IsInside" Value="true"/>
							<tt:VideoAnalytics><tt:Frame UtcTime="2026-11-16T12:00:01Z">
								<tt:Object ObjectId="7">
									<tt:Appearance><tt:Shape><tt:BoundingBox left="-0.50" top="0.60" right="0.00" bottom="-0.20"/></tt:Shape></tt:Appearance>
									<tt:Extension><tt:ObjectClassification><tt:Type>Human</tt:Type><tt:Likelihood>0.93</tt:Likelihood></tt:ObjectClassification></tt:Extension>
								</tt:Object>
								<tt:Object ObjectId="8">
									<tt:Appearance><tt:Shape><tt:BoundingBox left="0.10" top="0.20" right="0.70" bottom="-0.60"/></tt:Shape></tt:Appearance>
									<tt:Extension><tt:ObjectClassification><tt:Type>Vehicle</tt:Type><tt:Likelihood>0.86</tt:Likelihood></tt:ObjectClassification></tt:Extension>
								</tt:Object>
							</tt:Frame></tt:VideoAnalytics>
						</tt:Data>
					</tt:Message></wsnt:Message>
				</wsnt:NotificationMessage>
			</tev:PullMessagesResponse>`), nil
		case strings.Contains(req, "Unsubscribe"):
			return soapResp(200, `<wsnt:UnsubscribeResponse xmlns:wsnt="http://docs.oasis-open.org/wsn/b-2"/>`), nil
		default:
			return soapResp(400, `<s:Fault/>`), nil
		}
	})

	res, err := Probe(context.Background(), tr, ProbeRequest{Host: "192.0.2.225", Username: "admin", Password: "pw"}, now)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	caps := map[string]CapabilityStatus{}
	for _, c := range res.Capabilities {
		caps[c.Capability] = c.Status
		if strings.Contains(c.Evidence, "secret") {
			t.Fatalf("credential leaked in capability evidence: %s", c.Evidence)
		}
	}
	if caps["onvif.profile_g.search"] != CapConfirmed || caps["onvif.profile_g.replay"] != CapConfirmed || caps["onvif.profile_g"] != CapConfirmed {
		t.Fatalf("expected Profile G capabilities confirmed, got %+v", caps)
	}

	c := NewClient(tr, Config{Credentials: NewCredentials("admin", "pw")})
	ep, _ := ParseEndpoint("http://192.0.2.225/sub1")
	notifs, err := NewEventsClient(c, ep).PullMessages(context.Background(), ep, 5, 10)
	if err != nil || len(notifs) != 1 || len(notifs[0].Detections) != 2 {
		t.Fatalf("expected 1 notification with 2 objects, got notifs=%+v err=%v", notifs, err)
	}
	cls := ClassifyNotification(notifs[0])
	if !cls.Active || cls.Type != "person" || cls.Zone != "CocheraZona1" || len(cls.BBox) != 4 {
		t.Fatalf("unexpected classification: %+v", cls)
	}
	// [-0.50..0.00] -> x=0.25, w=0.25; [top=0.60, bottom=-0.20] -> y=0.20, h=0.40
	if math.Abs(cls.BBox[0]-0.25) > 0.01 || math.Abs(cls.BBox[1]-0.20) > 0.01 || math.Abs(cls.BBox[2]-0.25) > 0.01 || math.Abs(cls.BBox[3]-0.40) > 0.01 {
		t.Fatalf("unexpected normalized ONVIF bbox: %+v", cls.BBox)
	}
}
