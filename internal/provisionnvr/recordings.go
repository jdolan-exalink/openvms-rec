package provisionnvr

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// RecordSegment is a stretch of video the recorder holds for one channel.
type RecordSegment struct {
	// Type is what triggered the recording: MOTION, SCHEDULE, MANUAL and so on.
	Type  string
	Start time.Time
	End   time.Time
}

// maxRecordLogSpan bounds one record-log query; the recorder's own client asks for one day at a time.
const maxRecordLogSpan = 31 * 24 * time.Hour

// ErrRecordLogRange reports a record-log query with an invalid or too wide time range.
var ErrRecordLogRange = errors.New("the time range of the record log is invalid or too wide")

const recTypes = "MOTION SCHEDULE SENSOR MANUAL INTELLIGENT POS NORMALALL FACEDETECTION FACEMATCH VEHICLE TRIPWIRE INVADE AOIENTRY AOILEAVE ITEMCARE CROWDDENSITY EXCEPTION FIREPOINT TEMPERATURE"

const recordTimeLayout = "2006-01-02 15:04:05"

// RecordLog lists the recorded segments of a channel between from and to (queryChlRecLog). channelID is the
// channel GUID as OpenVMS stores it, with or without braces. Segments come in UTC, oldest first as the
// recorder sends them. The query is read-only.
func (c *Client) RecordLog(ctx context.Context, channelID string, from, to time.Time) ([]RecordSegment, error) {
	if !to.After(from) || to.Sub(from) > maxRecordLogSpan {
		return nil, fmt.Errorf("%w", ErrRecordLogRange)
	}
	id := trimID(channelID)
	if id == "" || strings.ContainsAny(id, "<>&'\"") {
		return nil, fmt.Errorf("%w: channel id", ErrInvalidEndpoint)
	}
	var types strings.Builder
	var enums strings.Builder
	for _, t := range strings.Fields(recTypes) {
		types.WriteString("<item>" + t + "</item>")
		enums.WriteString("<enum>" + t + "</enum>")
	}
	f, t := from.UTC().Format(recordTimeLayout), to.UTC().Format(recordTimeLayout)
	// The times go both as plain and as "Ex" fields, always in UTC; that is what the recorder answered correctly.
	content := "<types><recType>" + enums.String() + "</recType></types>" +
		"<requireField><chl/><recList><item><recType/><startTime/><endTime/><size/></item></recList></requireField>" +
		"<condition><modeType>modeOne</modeType>" +
		"<startTime>" + f + "</startTime><endTime>" + t + "</endTime>" +
		"<startTimeEx>" + f + "</startTimeEx><endTimeEx>" + t + "</endTimeEx>" +
		"<recType type='list'><itemType type='recType'/>" + types.String() + "</recType>" +
		"<chl id='{" + id + "}'></chl></condition>"
	var v struct {
		RecList struct {
			TimeZone string `xml:"timeZone,attr"`
			Items    []struct {
				Type  string `xml:"recType"`
				Start string `xml:"startTime"`
				End   string `xml:"endTime"`
			} `xml:"item"`
		} `xml:"content>recList"`
	}
	raw, err := c.Query(ctx, "queryChlRecLog", content)
	if err != nil {
		return nil, err
	}
	if err := xml.Unmarshal(raw, &v); err != nil {
		return nil, &Error{Class: ClassUnsupported, Command: "queryChlRecLog", Err: err}
	}
	if tz := v.RecList.TimeZone; tz != "" && !strings.EqualFold(tz, "UTC") {
		return nil, &Error{Class: ClassUnsupported, Command: "queryChlRecLog", Err: fmt.Errorf("the record log is in time zone %q, not UTC", tz)}
	}
	out := make([]RecordSegment, 0, len(v.RecList.Items))
	for _, it := range v.RecList.Items {
		s, e1 := time.ParseInLocation(recordTimeLayout, strings.TrimSpace(it.Start), time.UTC)
		e, e2 := time.ParseInLocation(recordTimeLayout, strings.TrimSpace(it.End), time.UTC)
		if e1 != nil || e2 != nil || !e.After(s) {
			continue // a malformed or empty segment is not worth failing the timeline for
		}
		out = append(out, RecordSegment{Type: it.Type, Start: s, End: e})
	}
	return out, nil
}

// UTCOffset is how far the recorder's local time is from UTC (queryTimeCfg). Its replay URLs take local
// time, so an offset that is guessed would play the wrong moment. The recorder's time zone ("ART3": three
// hours behind) is used when it states one without daylight saving; otherwise the offset is read from its
// current local time against ours and rounded to 15 minutes, which holds only while its clock is within
// seven minutes of the real time.
func (c *Client) UTCOffset(ctx context.Context) (time.Duration, error) {
	return c.utcOffsetAt(ctx, time.Now())
}

func (c *Client) utcOffsetAt(ctx context.Context, now time.Time) (time.Duration, error) {
	var v struct {
		Current  string `xml:"content>synchronizeInfo>currentTime"`
		Date     string `xml:"content>formatInfo>date"`
		Time     string `xml:"content>formatInfo>time"`
		Zone     string `xml:"content>timezoneInfo>timeZone"`
		Daylight string `xml:"content>timezoneInfo>daylightSwitch"`
	}
	if err := c.read(ctx, "queryTimeCfg", "", &v); err != nil {
		return 0, err
	}
	if !strings.EqualFold(strings.TrimSpace(v.Daylight), "true") {
		if off, ok := parsePOSIXOffset(strings.TrimSpace(v.Zone)); ok {
			return off, nil
		}
	}
	local, err := parseDeviceTime(strings.TrimSpace(v.Current), strings.TrimSpace(v.Date), strings.TrimSpace(v.Time))
	if err != nil {
		return 0, &Error{Class: ClassUnsupported, Command: "queryTimeCfg", Err: err}
	}
	diff := local.Sub(now.UTC().Truncate(time.Second))
	const step = 15 * time.Minute
	off := diff.Round(step)
	if off < -14*time.Hour || off > 14*time.Hour {
		return 0, &Error{Class: ClassUnsupported, Command: "queryTimeCfg", Err: errors.New("the recorder's clock is not within a time zone of now")}
	}
	return off, nil
}

// parseDeviceTime reads the recorder's local time, which it prints in the date and time formats its user chose.
func parseDeviceTime(current, dateFormat, timeFormat string) (time.Time, error) {
	dateLayouts := map[string]string{"year-month-day": "2006/01/02", "month-day-year": "01/02/2006", "day-month-year": "02/01/2006"}
	timeLayouts := map[string]string{"24": "15:04:05", "12": "03:04:05 PM"}
	dl, ok1 := dateLayouts[dateFormat]
	tl, ok2 := timeLayouts[timeFormat]
	if !ok1 || !ok2 {
		return time.Time{}, errors.New("unknown date or time format")
	}
	t, err := time.ParseInLocation(dl+" "+tl, current, time.UTC)
	if err != nil {
		return time.Time{}, errors.New("unreadable current time")
	}
	return t, nil
}

// ReplayPath is the path and query that plays a stretch of a channel's recording, in the recorder's own
// URL format (as ONVIF GetReplayUri returns it). start is converted to the recorder's local time with
// offset; duration is rounded up to whole seconds and at least one second.
func ReplayPath(channel int, stream Stream, start time.Time, offset time.Duration, duration time.Duration) string {
	local := start.UTC().Add(offset)
	secs := int((duration + time.Second - 1) / time.Second)
	if secs < 1 {
		secs = 1
	}
	return fmt.Sprintf("/chID=%d&date=%s&time=%s&timelen=%d&streamType=%s&linkType=tcp", channel, local.Format("2006-01-02"), local.Format("15:04:05"), secs, stream)
}

// parsePOSIXOffset reads a POSIX style time zone such as "ART3" (three hours west of UTC), "CET-1" or
// "IST-5:30" and returns its offset from UTC. Zones with daylight saving rules ("EST5EDT") are not read.
func parsePOSIXOffset(zone string) (time.Duration, bool) {
	m := posixZone.FindStringSubmatch(zone)
	if m == nil {
		return 0, false
	}
	hours, _ := strconv.Atoi(m[2])
	minutes := 0
	if m[3] != "" {
		minutes, _ = strconv.Atoi(m[3])
	}
	if hours > 14 || minutes > 59 {
		return 0, false
	}
	west := time.Duration(hours)*time.Hour + time.Duration(minutes)*time.Minute
	if m[1] == "-" {
		return west, true // "CET-1" is one hour east of UTC
	}
	return -west, true
}

var posixZone = regexp.MustCompile(`^[A-Za-z]{3,6}([+-]?)(\d{1,2})(?::(\d{2}))?$`)
