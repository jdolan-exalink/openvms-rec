package provisionnvr

import (
	"context"
	"encoding/xml"
	"strconv"
	"strings"
)

// flag reads the "true"/"false" strings the recorder uses; anything else is false.
type flag bool

func (f *flag) UnmarshalText(b []byte) error {
	*f = flag(strings.EqualFold(strings.TrimSpace(string(b)), "true"))
	return nil
}

// number reads an integer field; empty or malformed text is 0.
type number int64

func (n *number) UnmarshalText(b []byte) error {
	v, _ := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	*n = number(v)
	return nil
}

// trimID drops the braces the recorder puts around GUIDs: "{0000-...}" -> "0000-...".
func trimID(s string) string { return strings.Trim(strings.TrimSpace(s), "{}") }

func (c *Client) read(ctx context.Context, cmd, content string, into any) error {
	raw, err := c.Query(ctx, cmd, content)
	if err != nil {
		return err
	}
	if err := xml.Unmarshal(raw, into); err != nil {
		return &Error{Class: ClassUnsupported, Command: cmd, Err: err}
	}
	return nil
}

// Activated reports whether the recorder is set up (queryActivationStatus). It needs no login, so it
// is how a recorder of this platform is recognised before any credential is sent.
func (c *Client) Activated(ctx context.Context) (bool, error) {
	raw, err := c.QueryUnauthenticated(ctx, "queryActivationStatus")
	if err != nil {
		return false, err
	}
	var v struct {
		Activated flag `xml:"content>activated"`
	}
	if err := xml.Unmarshal(raw, &v); err != nil {
		return false, &Error{Class: ClassUnsupported, Command: "queryActivationStatus", Err: err}
	}
	return bool(v.Activated), nil
}

// DeviceInfo identifies the recorder. It deliberately leaves out the serial number, the hardware
// address and the account digest that queryBasicCfg also returns.
type DeviceInfo struct {
	Name            string
	Model           string
	HardwareModel   string
	SoftwareVersion string
	LaunchDate      string
}

// DeviceInfo reads the recorder's name, model and firmware (queryBasicCfg).
func (c *Client) DeviceInfo(ctx context.Context) (DeviceInfo, error) {
	var v struct {
		Name            string `xml:"content>name"`
		Model           string `xml:"content>productModel"`
		HardwareModel   string `xml:"content>TVTProductModel"`
		SoftwareVersion string `xml:"content>softwareVersion"`
		LaunchDate      string `xml:"content>launchDate"`
	}
	if err := c.read(ctx, "queryBasicCfg", "", &v); err != nil {
		return DeviceInfo{}, err
	}
	return DeviceInfo{Name: v.Name, Model: v.Model, HardwareModel: v.HardwareModel, SoftwareVersion: v.SoftwareVersion, LaunchDate: v.LaunchDate}, nil
}

// SystemCaps is what the recorder says about itself in querySystemCaps. These are claims of the
// device, not proof that the feature works end to end.
type SystemCaps struct {
	ChannelMax   int
	IPChannelMax int
	Talk         bool
	SNMP         bool
	PlateMatch   bool
	FaceMatch    bool
	POS          bool
	Recorder     bool
	PTZTrace     bool
	HTTPSConfig  bool
	AlarmServer  bool
}

// SystemCaps reads the recorder's declared capabilities.
func (c *Client) SystemCaps(ctx context.Context) (SystemCaps, error) {
	var v struct {
		ChannelMax   number `xml:"content>chlMaxCount"`
		IPChannelMax number `xml:"content>ipChlMaxCount"`
		Talk         flag   `xml:"content>supportTalk"`
		SNMP         flag   `xml:"content>supportSnmp"`
		PlateMatch   flag   `xml:"content>supportPlateMatch"`
		FaceMatch    flag   `xml:"content>supportFaceMatch"`
		POS          flag   `xml:"content>supportPOS"`
		Recorder     flag   `xml:"content>supportRecorder"`
		PTZTrace     flag   `xml:"content>supportPtzGroupAndTrace"`
		HTTPSConfig  flag   `xml:"content>supportHttpsConfig"`
		AlarmServer  flag   `xml:"content>supportAlarmServerConfig"`
	}
	if err := c.read(ctx, "querySystemCaps", "", &v); err != nil {
		return SystemCaps{}, err
	}
	return SystemCaps{
		ChannelMax: int(v.ChannelMax), IPChannelMax: int(v.IPChannelMax), Talk: bool(v.Talk), SNMP: bool(v.SNMP),
		PlateMatch: bool(v.PlateMatch), FaceMatch: bool(v.FaceMatch), POS: bool(v.POS), Recorder: bool(v.Recorder),
		PTZTrace: bool(v.PTZTrace), HTTPSConfig: bool(v.HTTPSConfig), AlarmServer: bool(v.AlarmServer),
	}, nil
}

// Channel is one camera slot of the recorder.
type Channel struct {
	// ID is the channel GUID without braces; it is stable and is what a camera is keyed on.
	ID           string
	Name         string
	Address      string
	Port         int
	Model        string
	Manufacturer string
	Protocol     string
	Kind         string
}

const nodeListQuery = `<types><nodeType><enum>chls</enum><enum>sensors</enum><enum>alarmOuts</enum></nodeType></types>` +
	`<nodeType type="nodeType">chls</nodeType><condition/>`

// Channels lists the camera channels configured on the recorder (queryNodeList).
func (c *Client) Channels(ctx context.Context) ([]Channel, error) {
	var v struct {
		Items []struct {
			ID           string `xml:"id,attr"`
			Name         string `xml:"name"`
			Address      string `xml:"ip"`
			Port         number `xml:"port"`
			Model        string `xml:"productModel"`
			Manufacturer string `xml:"manufacturer"`
			Protocol     string `xml:"protocolType"`
			Kind         string `xml:"chlType"`
		} `xml:"content>item"`
	}
	if err := c.read(ctx, "queryNodeList", nodeListQuery, &v); err != nil {
		return nil, err
	}
	out := make([]Channel, 0, len(v.Items))
	for _, it := range v.Items {
		out = append(out, Channel{ID: trimID(it.ID), Name: it.Name, Address: it.Address, Port: int(it.Port), Model: strings.TrimSpace(it.Model),
			Manufacturer: it.Manufacturer, Protocol: it.Protocol, Kind: it.Kind})
	}
	return out, nil
}

// ChannelStatus is the live state of one channel.
type ChannelStatus struct {
	ID          string
	Name        string
	Online      bool
	Motion      string
	Intelligent string
	Recording   string
}

// ChannelStatuses reads the live state of every channel (queryChlStatus).
func (c *Client) ChannelStatuses(ctx context.Context) ([]ChannelStatus, error) {
	var v struct {
		Items []struct {
			Chl struct {
				ID   string `xml:"id,attr"`
				Name string `xml:",chardata"`
			} `xml:"chl"`
			Online      flag   `xml:"online"`
			Motion      string `xml:"motionStatus"`
			Intelligent string `xml:"intelligentStatus"`
			Recording   string `xml:"recStatus"`
		} `xml:"content>item"`
	}
	if err := c.read(ctx, "queryChlStatus", "", &v); err != nil {
		return nil, err
	}
	out := make([]ChannelStatus, 0, len(v.Items))
	for _, it := range v.Items {
		out = append(out, ChannelStatus{ID: trimID(it.Chl.ID), Name: strings.TrimSpace(it.Chl.Name), Online: bool(it.Online),
			Motion: it.Motion, Intelligent: it.Intelligent, Recording: it.Recording})
	}
	return out, nil
}

// Disk is one disk of the recorder. Sizes are megabytes as the recorder reports them.
type Disk struct {
	ID    string
	Slot  int
	Model string
	// Status is read/write, read or bad (queryDiskStatus); empty when that list had no entry.
	Status      string
	SizeMB      int64
	FreeMB      int64
	RecordStart string
	RecordEnd   string
}

// Storage is the recorder's disks and whether it overwrites the oldest recordings.
type Storage struct {
	CycleRecord bool
	Disks       []Disk
}

// Storage reads the disks (queryStorageDevInfo and queryDiskStatus). The disk serial number is not kept.
func (c *Client) Storage(ctx context.Context) (Storage, error) {
	var info struct {
		CycleRecord flag `xml:"content>cycleRecord"`
		Disks       []struct {
			ID          string `xml:"id,attr"`
			Slot        number `xml:"slotIndex"`
			Model       string `xml:"model"`
			SizeMB      number `xml:"size"`
			FreeMB      number `xml:"freeSpace"`
			RecordStart string `xml:"recStartDate"`
			RecordEnd   string `xml:"recEndDate"`
		} `xml:"content>diskList>item"`
	}
	if err := c.read(ctx, "queryStorageDevInfo", "", &info); err != nil {
		return Storage{}, err
	}
	var status struct {
		Items []struct {
			ID     string `xml:"id,attr"`
			Status string `xml:"diskStatus"`
		} `xml:"content>item"`
	}
	statusByID := map[string]string{}
	if err := c.read(ctx, "queryDiskStatus", "", &status); err == nil {
		for _, it := range status.Items {
			statusByID[trimID(it.ID)] = it.Status
		}
	}
	out := Storage{CycleRecord: bool(info.CycleRecord)}
	for _, d := range info.Disks {
		id := trimID(d.ID)
		out.Disks = append(out.Disks, Disk{ID: id, Slot: int(d.Slot), Model: d.Model, Status: statusByID[id], SizeMB: int64(d.SizeMB),
			FreeMB: int64(d.FreeMB), RecordStart: d.RecordStart, RecordEnd: d.RecordEnd})
	}
	return out, nil
}

// Services is how the recorder is configured to be reached by other software.
type Services struct {
	// ONVIFSwitch is the ONVIF server switch; ONVIFEnabled is the licence/feature flag next to it.
	ONVIFSwitch  bool
	ONVIFEnabled bool
	RTSPOn       bool
	RTSPPort     int
	RTSPAuth     string
	RTSPAnon     bool
	APIServerOn  bool
	HTTPPort     int
	HTTPSPort    int
	HTTPSOn      bool
	NetPort      int
}

// Services reads the ONVIF, RTSP, API server and port settings (queryOnvifCfg, queryRTSPServer,
// queryApiServer, queryNetPortCfg). A setting that cannot be read stays at its zero value.
func (c *Client) Services(ctx context.Context) (Services, error) {
	var out Services
	var onvif struct {
		Enable flag `xml:"content>enable"`
		Switch flag `xml:"content>switch"`
	}
	if err := c.read(ctx, "queryOnvifCfg", "", &onvif); err != nil {
		return Services{}, err
	}
	out.ONVIFEnabled, out.ONVIFSwitch = bool(onvif.Enable), bool(onvif.Switch)
	var rtsp struct {
		On   flag   `xml:"content>rtspServerSwitch"`
		Port number `xml:"content>rtspPort"`
		Anon flag   `xml:"content>anonymousAccess"`
		Auth string `xml:"content>rtspAuthType"`
	}
	if err := c.read(ctx, "queryRTSPServer", "", &rtsp); err == nil {
		out.RTSPOn, out.RTSPPort, out.RTSPAnon, out.RTSPAuth = bool(rtsp.On), int(rtsp.Port), bool(rtsp.Anon), rtsp.Auth
	}
	var api struct {
		On flag `xml:"content>apiserverSwitch"`
	}
	if err := c.read(ctx, "queryApiServer", "", &api); err == nil {
		out.APIServerOn = bool(api.On)
	}
	var ports struct {
		HTTP    number `xml:"content>httpPort"`
		HTTPS   number `xml:"content>httpsPort"`
		HTTPSOn flag   `xml:"content>httpsSwitch"`
		Net     number `xml:"content>netPort"`
	}
	if err := c.read(ctx, "queryNetPortCfg", "", &ports); err == nil {
		out.HTTPPort, out.HTTPSPort, out.HTTPSOn, out.NetPort = int(ports.HTTP), int(ports.HTTPS), bool(ports.HTTPSOn), int(ports.Net)
	}
	return out, nil
}
