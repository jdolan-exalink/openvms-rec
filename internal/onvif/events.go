package onvif

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

const (
	eventsNamespace = "http://www.onvif.org/ver10/events/wsdl"
	wsntNamespace   = "http://docs.oasis-open.org/wsn/b-2"
)

// EventsClient provides ONVIF WS-BaseNotification and PullPoint event operations.
type EventsClient struct {
	client   *Client
	endpoint Endpoint
}

func NewEventsClient(client *Client, endpoint Endpoint) *EventsClient {
	return &EventsClient{client: client, endpoint: endpoint}
}

// maxEventTopics bounds how many topics one device may list; a longer list is cut, not an error.
const maxEventTopics = 200

type EventProperties struct {
	TopicSet []string `json:"topic_set"`
}

type PullPointSubscription struct {
	SubscriptionAddress string    `json:"subscription_address"`
	CurrentTime         time.Time `json:"current_time"`
	TerminationTime     time.Time `json:"termination_time"`
}

type EventNotification struct {
	Topic     string            `json:"topic"`
	Timestamp time.Time         `json:"timestamp"`
	Data      map[string]string `json:"data"`
}

func (e *EventsClient) GetEventProperties(ctx context.Context) (EventProperties, error) {
	data, err := e.call(ctx, e.endpoint, "GetEventProperties", "", ReadOnly)
	if err != nil {
		return EventProperties{}, err
	}
	return EventProperties{TopicSet: parseTopicSet(data)}, nil
}

// wstopNamespace is where the topic marker attribute lives.
const wstopNamespace = "http://docs.oasis-open.org/wsn/t-1"

// parseTopicSet lists the topics a device advertises as full paths (for example
// RuleEngine/LineDetector/Crossed), sorted, without duplicates and at most maxEventTopics of them.
// In a TopicSet only the elements marked wstop:topic="true" are topics; the rest are namespaces of
// the tree, and what sits inside a topic (its message description) is never a topic.
func parseTopicSet(data []byte) []string {
	decoder := xml.NewDecoder(bytes.NewReader(data))
	var (
		path     []string
		inSet    bool
		setDepth int
		depth    int
		seen     = map[string]bool{}
		topics   []string
	)
	for {
		token, err := decoder.Token()
		if err != nil {
			break
		}
		switch t := token.(type) {
		case xml.StartElement:
			depth++
			if !inSet {
				if t.Name.Local == "TopicSet" {
					inSet, setDepth = true, depth
				}
				continue
			}
			path = append(path, t.Name.Local)
			if !isTopicElement(t) {
				continue
			}
			topic := strings.Join(path, "/")
			if len(topic) <= 200 && !seen[topic] && len(topics) < maxEventTopics {
				seen[topic] = true
				topics = append(topics, topic)
			}
			// What a topic contains describes its messages; none of it is a topic.
			_ = decoder.Skip()
			path = path[:len(path)-1]
			depth--
		case xml.EndElement:
			if inSet && depth == setDepth {
				inSet, path = false, nil
			} else if inSet && len(path) > 0 {
				path = path[:len(path)-1]
			}
			depth--
		}
	}
	sort.Strings(topics)
	return topics
}

func isTopicElement(el xml.StartElement) bool {
	for _, attr := range el.Attr {
		if attr.Name.Local == "topic" && attr.Name.Space == wstopNamespace && strings.EqualFold(attr.Value, "true") {
			return true
		}
	}
	return false
}

func (e *EventsClient) CreatePullPointSubscription(ctx context.Context, initialTermination string) (PullPointSubscription, error) {
	if initialTermination == "" {
		initialTermination = "PT10M"
	}
	content := `<tev:InitialTerminationTime>` + initialTermination + `</tev:InitialTerminationTime>`
	data, err := e.call(ctx, e.endpoint, "CreatePullPointSubscription", content, Mutating)
	if err != nil {
		return PullPointSubscription{}, err
	}

	var response struct {
		Address string `xml:"SubscriptionReference>Address"`
	}
	if err := decodeEventsOperation(data, "CreatePullPointSubscriptionResponse", &response); err != nil {
		return PullPointSubscription{}, &CallError{Status: StatusFailed, Code: "invalid_pullpoint_subscription"}
	}

	return PullPointSubscription{
		SubscriptionAddress: response.Address,
	}, nil
}

func (e *EventsClient) PullMessages(ctx context.Context, subEndpoint Endpoint, timeoutSeconds, messageLimit int) ([]EventNotification, error) {
	if timeoutSeconds <= 0 {
		timeoutSeconds = 5
	}
	if messageLimit <= 0 {
		messageLimit = 10
	}
	content := fmt.Sprintf(`<tev:Timeout>PT%dS</tev:Timeout><tev:MessageLimit>%d</tev:MessageLimit>`, timeoutSeconds, messageLimit)
	data, err := e.call(ctx, subEndpoint, "PullMessages", content, Mutating)
	if err != nil {
		return nil, err
	}

	var notifs []EventNotification
	decoder := xml.NewDecoder(bytes.NewReader(data))
	for {
		token, err := decoder.Token()
		if err != nil {
			break
		}
		start, ok := token.(xml.StartElement)
		if !ok || start.Name.Local != "NotificationMessage" {
			continue
		}
		var msg struct {
			Topic   string `xml:"Topic"`
			Message struct {
				UtcTime time.Time `xml:"UtcTime,attr"`
				Data    struct {
					SimpleItems []struct {
						Name  string `xml:"Name,attr"`
						Value string `xml:"Value,attr"`
					} `xml:"SimpleItem"`
				} `xml:"Data"`
			} `xml:"Message"`
		}
		if err := decoder.DecodeElement(&msg, &start); err == nil {
			dataMap := make(map[string]string)
			for _, item := range msg.Message.Data.SimpleItems {
				dataMap[item.Name] = item.Value
			}
			notifs = append(notifs, EventNotification{
				Topic:     msg.Topic,
				Timestamp: msg.Message.UtcTime,
				Data:      dataMap,
			})
		}
	}

	return notifs, nil
}

func decodeEventsOperation(data []byte, name string, target any) error {
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
		if !ok || start.Name.Space != eventsNamespace || start.Name.Local != name {
			continue
		}
		return decoder.DecodeElement(target, &start)
	}
}

func (e *EventsClient) call(ctx context.Context, ep Endpoint, action, content string, op Operation) ([]byte, error) {
	body := `<tev:` + action + ` xmlns:tev="` + eventsNamespace + `">` + content + `</tev:` + action + `>`
	return e.client.Call(ctx, ep, eventsNamespace+"/"+action, body, op)
}
