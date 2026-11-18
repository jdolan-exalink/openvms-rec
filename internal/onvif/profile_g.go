package onvif

import (
	"context"
	"encoding/xml"
	"fmt"
	"strings"
	"time"
)

const (
	searchNamespace    = "http://www.onvif.org/ver10/search/wsdl"
	replayNamespace    = "http://www.onvif.org/ver10/replay/wsdl"
	recordingNamespace = "http://www.onvif.org/ver10/recording/wsdl"
)

// RecordingSummary reports the earliest/latest SD recordings reported by ONVIF Profile G Search.
type RecordingSummary struct {
	DataFrom         time.Time `json:"data_from,omitzero"`
	DataUntil        time.Time `json:"data_until,omitzero"`
	NumberRecordings int       `json:"number_recordings"`
}

// RecordingInfo describes one camera recording token on edge storage (microSD).
type RecordingInfo struct {
	RecordingToken    string    `json:"recording_token"`
	SourceID          string    `json:"source_id,omitempty"`
	EarliestRecording time.Time `json:"earliest_recording,omitzero"`
	LatestRecording   time.Time `json:"latest_recording,omitzero"`
	Content           string    `json:"content,omitempty"`
	HasVideo          bool      `json:"has_video"`
}

// SearchClient implements read-only ONVIF Profile G Search service calls.
type SearchClient struct {
	client   *Client
	endpoint Endpoint
}

func NewSearchClient(client *Client, endpoint Endpoint) *SearchClient {
	return &SearchClient{client: client, endpoint: endpoint}
}

func (s *SearchClient) GetRecordingSummary(ctx context.Context) (RecordingSummary, error) {
	body := `<tse:GetRecordingSummary xmlns:tse="` + searchNamespace + `"/>`
	data, err := s.client.Call(ctx, s.endpoint, searchNamespace+"/GetRecordingSummary", body)
	if err != nil {
		return RecordingSummary{}, err
	}
	var resp struct {
		Summary struct {
			DataFrom         string `xml:"DataFrom"`
			DataUntil        string `xml:"DataUntil"`
			NumberRecordings int    `xml:"NumberRecordings"`
		} `xml:"Summary"`
	}
	if err := decodeLocalOperation(data, "GetRecordingSummaryResponse", &resp); err != nil {
		return RecordingSummary{}, err
	}
	var out RecordingSummary
	out.NumberRecordings = resp.Summary.NumberRecordings
	out.DataFrom, _ = time.Parse(time.RFC3339Nano, strings.TrimSpace(resp.Summary.DataFrom))
	out.DataUntil, _ = time.Parse(time.RFC3339Nano, strings.TrimSpace(resp.Summary.DataUntil))
	return out, nil
}

// FindRecordings starts an ONVIF Search session and returns its SearchToken.
func (s *SearchClient) FindRecordings(ctx context.Context, maxMatches int) (string, error) {
	if maxMatches <= 0 {
		maxMatches = 20
	}
	body := fmt.Sprintf(`<tse:FindRecordings xmlns:tse="%s"><tse:Scope><tt:IncludedSources xmlns:tt="%s"/><tse:RecordingInformationFilter/></tse:Scope><tse:MaxMatches>%d</tse:MaxMatches><tse:KeepAliveTime>PT30S</tse:KeepAliveTime></tse:FindRecordings>`,
		searchNamespace, schemaNamespace, maxMatches)
	data, err := s.client.Call(ctx, s.endpoint, searchNamespace+"/FindRecordings", body)
	if err != nil {
		return "", err
	}
	var resp struct {
		SearchToken string `xml:"SearchToken"`
	}
	if err := decodeLocalOperation(data, "FindRecordingsResponse", &resp); err != nil {
		return "", err
	}
	return strings.TrimSpace(resp.SearchToken), nil
}

// GetRecordingSearchResults retrieves discovered recordings for searchToken.
func (s *SearchClient) GetRecordingSearchResults(ctx context.Context, searchToken string, maxResults int) ([]RecordingInfo, string, error) {
	if maxResults <= 0 {
		maxResults = 20
	}
	body := fmt.Sprintf(`<tse:GetRecordingSearchResults xmlns:tse="%s"><tse:SearchToken>%s</tse:SearchToken><tse:MinResults>1</tse:MinResults><tse:MaxResults>%d</tse:MaxResults><tse:WaitTime>PT4S</tse:WaitTime></tse:GetRecordingSearchResults>`,
		searchNamespace, escapeXML(searchToken), maxResults)
	data, err := s.client.Call(ctx, s.endpoint, searchNamespace+"/GetRecordingSearchResults", body)
	if err != nil {
		return nil, "", err
	}
	var resp struct {
		ResultList struct {
			SearchState string `xml:"SearchState"`
			Recordings  []struct {
				Token  string `xml:"RecordingToken"`
				Source struct {
					SourceID string `xml:"SourceId"`
				} `xml:"Source"`
				Content  string `xml:"Content"`
				Earliest string `xml:"EarliestRecording"`
				Latest   string `xml:"LatestRecording"`
				Tracks   []struct {
					TrackType string `xml:"TrackType"`
				} `xml:"Track"`
			} `xml:"RecordingInformation"`
		} `xml:"ResultList"`
	}
	if err := decodeLocalOperation(data, "GetRecordingSearchResultsResponse", &resp); err != nil {
		return nil, "", err
	}
	var out []RecordingInfo
	for _, r := range resp.ResultList.Recordings {
		tok := strings.TrimSpace(r.Token)
		if tok == "" {
			continue
		}
		ri := RecordingInfo{
			RecordingToken: tok,
			SourceID:       strings.TrimSpace(r.Source.SourceID),
			Content:        strings.TrimSpace(r.Content),
		}
		ri.EarliestRecording, _ = time.Parse(time.RFC3339Nano, strings.TrimSpace(r.Earliest))
		ri.LatestRecording, _ = time.Parse(time.RFC3339Nano, strings.TrimSpace(r.Latest))
		for _, tr := range r.Tracks {
			if strings.EqualFold(strings.TrimSpace(tr.TrackType), "Video") || tr.TrackType == "" {
				ri.HasVideo = true
			}
		}
		if len(r.Tracks) == 0 {
			ri.HasVideo = true
		}
		out = append(out, ri)
	}
	return out, strings.TrimSpace(resp.ResultList.SearchState), nil
}

func (s *SearchClient) EndSearch(ctx context.Context, searchToken string) error {
	if strings.TrimSpace(searchToken) == "" {
		return nil
	}
	body := fmt.Sprintf(`<tse:EndSearch xmlns:tse="%s"><tse:SearchToken>%s</tse:SearchToken></tse:EndSearch>`,
		searchNamespace, escapeXML(searchToken))
	_, err := s.client.Call(ctx, s.endpoint, searchNamespace+"/EndSearch", body)
	return err
}

// ListRecordings runs a bounded FindRecordings + GetRecordingSearchResults + EndSearch sequence.
func (s *SearchClient) ListRecordings(ctx context.Context, maxResults int) ([]RecordingInfo, error) {
	if maxResults <= 0 {
		maxResults = 20
	}
	tok, err := s.FindRecordings(ctx, maxResults)
	if err != nil {
		return nil, err
	}
	recs, _, resErr := s.GetRecordingSearchResults(ctx, tok, maxResults)
	eCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	_ = s.EndSearch(eCtx, tok)
	cancel()
	if len(recs) > 0 || resErr != nil {
		return recs, resErr
	}
	// Retry once with minimal <Scope/> for cameras (e.g. TVT/Provision) that return
	// empty matches when empty IncludedSources/RecordingInformationFilter tags are sent.
	body := fmt.Sprintf(`<tse:FindRecordings xmlns:tse="%s"><tse:Scope/><tse:MaxMatches>%d</tse:MaxMatches><tse:KeepAliveTime>PT30S</tse:KeepAliveTime></tse:FindRecordings>`,
		searchNamespace, maxResults)
	if data, fErr := s.client.Call(ctx, s.endpoint, searchNamespace+"/FindRecordings", body); fErr == nil {
		var resp struct {
			SearchToken string `xml:"SearchToken"`
		}
		if decodeLocalOperation(data, "FindRecordingsResponse", &resp) == nil && strings.TrimSpace(resp.SearchToken) != "" {
			tok2 := strings.TrimSpace(resp.SearchToken)
			recs, _, resErr = s.GetRecordingSearchResults(ctx, tok2, maxResults)
			eCtx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
			_ = s.EndSearch(eCtx2, tok2)
			cancel2()
		}
	}
	return recs, resErr
}

// RecordingJobInfo describes an ONVIF Profile G recording job configured on the camera.
type RecordingJobInfo struct {
	JobToken       string `json:"job_token"`
	RecordingToken string `json:"recording_token,omitempty"`
	Mode           string `json:"mode,omitempty"`
	State          string `json:"state,omitempty"`
}

// RecordingClient implements read-only ONVIF Recording service inspection calls.
type RecordingClient struct {
	client   *Client
	endpoint Endpoint
}

func NewRecordingClient(client *Client, endpoint Endpoint) *RecordingClient {
	return &RecordingClient{client: client, endpoint: endpoint}
}

func (r *RecordingClient) GetRecordings(ctx context.Context) ([]RecordingInfo, error) {
	body := `<trc:GetRecordings xmlns:trc="` + recordingNamespace + `"/>`
	data, err := r.client.Call(ctx, r.endpoint, recordingNamespace+"/GetRecordings", body)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Items []struct {
			Token string `xml:"RecordingToken"`
			Cfg   struct {
				Source struct {
					SourceID string `xml:"SourceId"`
				} `xml:"Source"`
				Content string `xml:"Content"`
			} `xml:"Configuration"`
			Tracks []struct {
				TrackToken string `xml:"TrackToken"`
				Cfg        struct {
					TrackType string `xml:"TrackType"`
				} `xml:"Configuration"`
			} `xml:"Tracks>Track"`
		} `xml:"RecordingItem"`
	}
	if err := decodeLocalOperation(data, "GetRecordingsResponse", &resp); err != nil {
		return nil, err
	}
	var out []RecordingInfo
	for _, it := range resp.Items {
		tok := strings.TrimSpace(it.Token)
		if tok == "" {
			continue
		}
		ri := RecordingInfo{
			RecordingToken: tok,
			SourceID:       strings.TrimSpace(it.Cfg.Source.SourceID),
			Content:        strings.TrimSpace(it.Cfg.Content),
		}
		for _, tr := range it.Tracks {
			if strings.EqualFold(strings.TrimSpace(tr.Cfg.TrackType), "Video") || tr.Cfg.TrackType == "" {
				ri.HasVideo = true
			}
		}
		if len(it.Tracks) == 0 {
			ri.HasVideo = true
		}
		out = append(out, ri)
	}
	return out, nil
}

func (r *RecordingClient) GetRecordingJobs(ctx context.Context) ([]RecordingJobInfo, error) {
	body := `<trc:GetRecordingJobs xmlns:trc="` + recordingNamespace + `"/>`
	data, err := r.client.Call(ctx, r.endpoint, recordingNamespace+"/GetRecordingJobs", body)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Jobs []struct {
			Token string `xml:"JobToken"`
			Cfg   struct {
				RecordingToken string `xml:"RecordingToken"`
				Mode           string `xml:"Mode"`
			} `xml:"JobConfiguration"`
			State string `xml:"State"`
		} `xml:"JobItem"`
	}
	if err := decodeLocalOperation(data, "GetRecordingJobsResponse", &resp); err != nil {
		return nil, err
	}
	var out []RecordingJobInfo
	for _, j := range resp.Jobs {
		out = append(out, RecordingJobInfo{
			JobToken:       strings.TrimSpace(j.Token),
			RecordingToken: strings.TrimSpace(j.Cfg.RecordingToken),
			Mode:           strings.TrimSpace(j.Cfg.Mode),
			State:          strings.TrimSpace(j.State),
		})
	}
	return out, nil
}

// ReplayClient implements ONVIF Profile G Replay service calls.
type ReplayClient struct {
	client   *Client
	endpoint Endpoint
}

func NewReplayClient(client *Client, endpoint Endpoint) *ReplayClient {
	return &ReplayClient{client: client, endpoint: endpoint}
}

// GetReplayURI asks the camera for the credential-free RTSP replay URI for recordingToken.
func (r *ReplayClient) GetReplayURI(ctx context.Context, recordingToken string) (string, error) {
	if strings.TrimSpace(recordingToken) == "" {
		return "", fmt.Errorf("recording token is required")
	}
	body := fmt.Sprintf(`<trp:GetReplayUri xmlns:trp="%s"><trp:StreamSetup><tt:Stream xmlns:tt="%s">RTP-Unicast</tt:Stream><tt:Transport xmlns:tt="%s"><tt:Protocol>RTSP</tt:Protocol></tt:Transport></trp:StreamSetup><trp:RecordingToken>%s</trp:RecordingToken></trp:GetReplayUri>`,
		replayNamespace, schemaNamespace, schemaNamespace, escapeXML(recordingToken))
	data, err := r.client.Call(ctx, r.endpoint, replayNamespace+"/GetReplayUri", body)
	if err != nil {
		return "", err
	}
	var resp struct {
		XMLName xml.Name
		Uri     string `xml:"Uri"`
	}
	if err := decodeLocalOperation(data, "GetReplayUriResponse", &resp); err != nil {
		return "", err
	}
	return SanitizeRTSPURI(strings.TrimSpace(resp.Uri)), nil
}
