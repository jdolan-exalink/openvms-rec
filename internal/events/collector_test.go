package events

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jdolan-exalink/openvms-rec/internal/db"
	"github.com/jdolan-exalink/openvms-rec/internal/onvif"
)

const env = `<?xml version="1.0"?><s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope" xmlns:tds="http://www.onvif.org/ver10/device/wsdl" xmlns:tev="http://www.onvif.org/ver10/events/wsdl" xmlns:wsnt="http://docs.oasis-open.org/wsn/b-2" xmlns:wsa="http://www.w3.org/2005/08/addressing" xmlns:tt="http://www.onvif.org/ver10/schema" xmlns:tns1="http://www.onvif.org/ver10/topics"><s:Body>%s</s:Body></s:Envelope>`

func note(topic, op, key, val, at string) string {
	return `<wsnt:NotificationMessage><wsnt:Topic Dialect="http://www.onvif.org/ver10/tev/topicExpression/ConcreteSet">` + topic + `</wsnt:Topic><wsnt:Message><tt:Message UtcTime="` + at + `" PropertyOperation="` + op + `"><tt:Source><tt:SimpleItem Name="VideoSourceConfigurationToken" Value="vsc1"/></tt:Source><tt:Data><tt:SimpleItem Name="` + key + `" Value="` + val + `"/></tt:Data></tt:Message></wsnt:Message></wsnt:NotificationMessage>`
}

func TestCollectorPullPoint(t *testing.T) {
	var pulls atomic.Int32
	at := time.Now().UTC().Format(time.RFC3339)
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body := string(b)
		reply := func(inner string) { fmt.Fprintf(w, env, inner) }
		switch {
		case strings.Contains(body, "GetSystemDateAndTime"):
			now := time.Now().UTC()
			reply(fmt.Sprintf(`<tds:GetSystemDateAndTimeResponse><tds:SystemDateAndTime><tt:UTCDateTime><tt:Time><tt:Hour>%d</tt:Hour><tt:Minute>%d</tt:Minute><tt:Second>%d</tt:Second></tt:Time><tt:Date><tt:Year>%d</tt:Year><tt:Month>%d</tt:Month><tt:Day>%d</tt:Day></tt:Date></tt:UTCDateTime></tds:SystemDateAndTime></tds:GetSystemDateAndTimeResponse>`,
				now.Hour(), now.Minute(), now.Second(), now.Year(), int(now.Month()), now.Day()))
		case !strings.Contains(body, "UsernameToken"):
			w.WriteHeader(http.StatusUnauthorized)
		case strings.Contains(body, "GetServices"):
			reply(`<tds:GetServicesResponse><tds:Service><tds:Namespace>http://www.onvif.org/ver10/events/wsdl</tds:Namespace><tds:XAddr>` + srv.URL + `/onvif/events</tds:XAddr></tds:Service></tds:GetServicesResponse>`)
		case strings.Contains(body, "CreatePullPointSubscription"):
			reply(`<tev:CreatePullPointSubscriptionResponse><tev:SubscriptionReference><wsa:Address>` + srv.URL + `/onvif/sub1</wsa:Address></tev:SubscriptionReference></tev:CreatePullPointSubscriptionResponse>`)
		case strings.Contains(body, "PullMessages"):
			if pulls.Add(1) == 1 {
				reply(`<tev:PullMessagesResponse>` +
					note("tns1:RuleEngine/CellMotionDetector/Motion", "Initialized", "IsMotion", "true", at) +
					note("tns1:RuleEngine/CellMotionDetector/Motion", "Changed", "IsMotion", "true", at) +
					note("tns1:RuleEngine/CellMotionDetector/Motion", "Changed", "IsMotion", "true", at) + // duplicate
					note("tns1:RuleEngine/CellMotionDetector/Motion", "Changed", "IsMotion", "false", at) +
					note("tns1:RuleEngine/LicensePlateDetector/Plate", "", "PlateNumber", "AB123CD", at) +
					`</tev:PullMessagesResponse>`)
				return
			}
			time.Sleep(50 * time.Millisecond)
			reply(`<tev:PullMessagesResponse/>`)
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	t.Cleanup(srv.Close)

	d, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	c := &Collector{DB: d, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Transport: http.DefaultTransport, PullTimeout: time.Second}
	c.Apply("patio", &Target{CameraID: "patio", DeviceURL: srv.URL + "/onvif/device_service", Username: "u", Password: "p"})
	deadline := time.Now().Add(5 * time.Second)
	for {
		st, _ := c.Status("patio")
		if st.Events >= 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	c.StopAll()

	rows, err := d.Query(`SELECT type, class, source FROM events WHERE camera_id='patio' ORDER BY type`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var typ, class, src string
		rows.Scan(&typ, &class, &src)
		got = append(got, typ+"/"+class+"/"+src)
	}
	want := []string{"lpr/AB123CD/onvif_events", "motion//onvif_events"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("events %v, want %v", got, want)
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		n   onvif.EventNotification
		ok  bool
		typ string
	}{
		{onvif.EventNotification{Topic: "tns1:VideoSource/MotionAlarm", Data: map[string]string{"State": "true"}}, true, "motion"},
		{onvif.EventNotification{Topic: "tns1:VideoSource/MotionAlarm", Data: map[string]string{"State": "false"}}, false, ""},
		{onvif.EventNotification{Topic: "tns1:RuleEngine/LineDetector/Crossed", Data: map[string]string{"ObjectId": "3"}}, true, "line_crossing"},
		{onvif.EventNotification{Topic: "tns1:RuleEngine/FieldDetector/ObjectsInside", Data: map[string]string{"IsInside": "true"}}, true, "intrusion"},
		{onvif.EventNotification{Topic: "tns1:Device/Trigger/DigitalInput", Data: map[string]string{"LogicalState": "true"}}, true, "onvif"},
		{onvif.EventNotification{Topic: "tns1:VideoSource/MotionAlarm", PropertyOperation: "Initialized", Data: map[string]string{"State": "true"}}, false, ""},
	}
	for _, c := range cases {
		e, ok := Classify(c.n)
		if ok != c.ok || (ok && e.Type != c.typ) {
			t.Errorf("%s %v: got %v %q", c.n.Topic, c.n.Data, ok, e.Type)
		}
	}
}
