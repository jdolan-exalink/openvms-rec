package onvif

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type eventsRoundTripperFunc func(*http.Request) (*http.Response, error)

func (f eventsRoundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestEventsClientOperations(t *testing.T) {
	endpoint, err := ParseEndpoint("http://192.168.1.100/onvif/events_service")
	if err != nil {
		t.Fatal(err)
	}

	transport := eventsRoundTripperFunc(func(r *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(r.Body)
		sBody := string(body)

		if strings.Contains(sBody, "CreatePullPointSubscription") {
			xmlResp := `<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope" xmlns:tev="http://www.onvif.org/ver10/events/wsdl" xmlns:wsa="http://www.w3.org/2005/08/addressing">
				<s:Body>
					<tev:CreatePullPointSubscriptionResponse>
						<tev:SubscriptionReference>
							<wsa:Address>http://192.168.1.100/onvif/events_service/sub_42</wsa:Address>
						</tev:SubscriptionReference>
					</tev:CreatePullPointSubscriptionResponse>
				</s:Body>
			</s:Envelope>`
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(xmlResp)),
				Header:     make(http.Header),
			}, nil
		}

		if strings.Contains(sBody, "PullMessages") {
			xmlResp := `<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope" xmlns:tev="http://www.onvif.org/ver10/events/wsdl" xmlns:wsnt="http://docs.oasis-open.org/wsn/b-2">
				<s:Body>
					<tev:PullMessagesResponse>
						<wsnt:NotificationMessage>
							<wsnt:Topic>RuleEngine/LineDetector/Crossed</wsnt:Topic>
							<wsnt:Message UtcTime="2026-10-14T12:05:00Z">
								<tt:Data xmlns:tt="http://www.onvif.org/ver10/schema">
									<tt:SimpleItem Name="State" Value="true"/>
								</tt:Data>
							</wsnt:Message>
						</wsnt:NotificationMessage>
					</tev:PullMessagesResponse>
				</s:Body>
			</s:Envelope>`
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(xmlResp)),
				Header:     make(http.Header),
			}, nil
		}

		return &http.Response{StatusCode: http.StatusBadRequest, Body: io.NopCloser(strings.NewReader(""))}, nil
	})

	client := NewClient(transport, Config{})
	eventsClient := NewEventsClient(client, endpoint)

	sub, err := eventsClient.CreatePullPointSubscription(context.Background(), "PT5M")
	if err != nil {
		t.Fatalf("CreatePullPointSubscription failed: %v", err)
	}
	if sub.SubscriptionAddress != "http://192.168.1.100/onvif/events_service/sub_42" {
		t.Errorf("unexpected subscription address: %s", sub.SubscriptionAddress)
	}

	subEndpoint, _ := ParseEndpoint(sub.SubscriptionAddress)
	messages, err := eventsClient.PullMessages(context.Background(), subEndpoint, 5, 10)
	if err != nil {
		t.Fatalf("PullMessages failed: %v", err)
	}
	if len(messages) != 1 || !strings.Contains(messages[0].Topic, "LineDetector") || messages[0].Data["State"] != "true" {
		t.Errorf("unexpected messages: %+v", messages)
	}
}

const getEventPropertiesResponse = `<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope" xmlns:tev="http://www.onvif.org/ver10/events/wsdl"
  xmlns:wsnt="http://docs.oasis-open.org/wsn/b-2" xmlns:wstop="http://docs.oasis-open.org/wsn/t-1"
  xmlns:tns1="http://www.onvif.org/ver10/topics" xmlns:tt="http://www.onvif.org/ver10/schema">
  <s:Body>
    <tev:GetEventPropertiesResponse>
      <tev:TopicNamespaceLocation>http://www.onvif.org/onvif/ver10/topics/topicns.xml</tev:TopicNamespaceLocation>
      <wsnt:FixedTopicSet>true</wsnt:FixedTopicSet>
      <wstop:TopicSet>
        <tns1:RuleEngine wstop:topic="false">
          <LineDetector wstop:topic="false">
            <Crossed wstop:topic="true">
              <tt:MessageDescription IsProperty="false">
                <tt:Source><tt:SimpleItemDescription Name="VideoSourceConfigurationToken" Type="tt:ReferenceToken"/></tt:Source>
                <tt:Data><tt:SimpleItemDescription Name="ObjectId" Type="tt:ObjectRefType"/></tt:Data>
              </tt:MessageDescription>
            </Crossed>
          </LineDetector>
          <FieldDetector wstop:topic="false">
            <ObjectsInside wstop:topic="true"><tt:MessageDescription IsProperty="true"/></ObjectsInside>
          </FieldDetector>
          <FaceDetector wstop:topic="false">
            <FaceDetected wstop:topic="true"><tt:MessageDescription IsProperty="false"/></FaceDetected>
          </FaceDetector>
        </tns1:RuleEngine>
        <tns1:VideoSource wstop:topic="false">
          <MotionAlarm wstop:topic="true"><tt:MessageDescription IsProperty="true"/></MotionAlarm>
        </tns1:VideoSource>
      </wstop:TopicSet>
      <wsnt:TopicExpressionDialect>http://www.onvif.org/ver10/tev/topicExpression/ConcreteSet</wsnt:TopicExpressionDialect>
    </tev:GetEventPropertiesResponse>
  </s:Body>
</s:Envelope>`

func eventPropertiesClient(t *testing.T, response string) *EventsClient {
	t.Helper()
	endpoint, err := ParseEndpoint("http://192.168.1.100/onvif/events_service")
	if err != nil {
		t.Fatal(err)
	}
	transport := eventsRoundTripperFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(response)), Header: make(http.Header)}, nil
	})
	return NewEventsClient(NewClient(transport, Config{}), endpoint)
}

func TestGetEventPropertiesReturnsTheFullPathOfEveryTopic(t *testing.T) {
	props, err := eventPropertiesClient(t, getEventPropertiesResponse).GetEventProperties(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"RuleEngine/FaceDetector/FaceDetected",
		"RuleEngine/FieldDetector/ObjectsInside",
		"RuleEngine/LineDetector/Crossed",
		"VideoSource/MotionAlarm",
	}
	if strings.Join(props.TopicSet, ",") != strings.Join(want, ",") {
		t.Fatalf("topics = %v, want %v (full paths, sorted, no message-description items)", props.TopicSet, want)
	}
}

func TestGetEventPropertiesIgnoresDuplicatesAndCapsTheList(t *testing.T) {
	var b strings.Builder
	b.WriteString(`<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope" xmlns:tev="http://www.onvif.org/ver10/events/wsdl" xmlns:wstop="http://docs.oasis-open.org/wsn/t-1"><s:Body><tev:GetEventPropertiesResponse><wstop:TopicSet>`)
	for i := 0; i < 400; i++ {
		b.WriteString(`<Group wstop:topic="false"><Dup wstop:topic="true"/></Group>`)
	}
	for i := 0; i < 400; i++ {
		b.WriteString(`<G` + strings.Repeat("x", i%5+1) + `><T` + strings.Repeat("y", i/5+1) + ` wstop:topic="true"/></G` + strings.Repeat("x", i%5+1) + `>`)
	}
	b.WriteString(`</wstop:TopicSet></tev:GetEventPropertiesResponse></s:Body></s:Envelope>`)
	props, err := eventPropertiesClient(t, b.String()).GetEventProperties(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(props.TopicSet) > maxEventTopics {
		t.Fatalf("topics = %d, want at most %d", len(props.TopicSet), maxEventTopics)
	}
	seen := map[string]bool{}
	for _, topic := range props.TopicSet {
		if seen[topic] {
			t.Fatalf("topic %q listed twice", topic)
		}
		seen[topic] = true
	}
}

func TestGetEventPropertiesWithoutATopicSetReturnsNoTopics(t *testing.T) {
	props, err := eventPropertiesClient(t, `<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope"><s:Body><x/></s:Body></s:Envelope>`).GetEventProperties(context.Background())
	if err != nil || len(props.TopicSet) != 0 {
		t.Fatalf("topics = %v, %v; want none", props.TopicSet, err)
	}
}
