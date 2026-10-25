package hdhr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Device is one HDHomeRun: the control protocol for tuners plus its HTTP API
// for device info and the channel lineup.
type Device struct {
	Host string

	ctl  *Client
	http *http.Client
	base string // http://host, or http://host:port when the port isn't 80
}

// NewDevice returns a device at host. host may carry a port for the control
// protocol (used by tests); the HTTP API is then expected on httpPort.
func NewDevice(host string, timeout time.Duration) *Device {
	h := host
	if hh, _, err := net.SplitHostPort(host); err == nil {
		h = hh
	}
	return &Device{
		Host: host,
		ctl:  NewClient(host, timeout),
		http: &http.Client{Timeout: timeout},
		base: "http://" + h,
	}
}

// SetHTTPBase overrides the HTTP API's base URL.
func (d *Device) SetHTTPBase(base string) { d.base = base }

func (d *Device) Close() error { return d.ctl.Close() }

// Info is the device's discover.json.
type Info struct {
	FriendlyName    string
	ModelNumber     string
	FirmwareName    string
	FirmwareVersion string
	DeviceID        string
	DeviceAuth      string // token for Silicondust's guide service; changes over time
	TunerCount      int
}

// LineupEntry is one channel of lineup.json.
type LineupEntry struct {
	GuideNumber string
	GuideName   string
	HD          int `json:",omitempty"`
	DRM         int `json:",omitempty"`
}

func (d *Device) Info(ctx context.Context) (Info, error) {
	var info Info
	err := d.getJSON(ctx, "/discover.json", &info)
	return info, err
}

func (d *Device) Lineup(ctx context.Context) ([]LineupEntry, error) {
	var lineup []LineupEntry
	err := d.getJSON(ctx, "/lineup.json", &lineup)
	return lineup, err
}

// GuideChannel is one channel of Silicondust's guide service, which knows each
// station's network and logo. The device's lineup only has call signs.
type GuideChannel struct {
	GuideNumber string
	GuideName   string
	Affiliate   string `json:",omitempty"`
	ImageURL    string `json:",omitempty"`
}

// GuideURL is Silicondust's guide endpoint, the one the HDHomeRun apps use.
var GuideURL = "https://api.hdhomerun.com/api/guide.php"

// Guide fetches the channel guide for this device from Silicondust, using the
// DeviceAuth from its discover.json. Program listings are dropped.
func (d *Device) Guide(ctx context.Context, deviceAuth string) ([]GuideChannel, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, GuideURL+"?DeviceAuth="+url.QueryEscape(deviceAuth), nil)
	if err != nil {
		return nil, err
	}
	// The service refuses Go's default User-Agent.
	req.Header.Set("User-Agent", "hdhr-monitor/1.0")
	c := &http.Client{Timeout: 30 * time.Second}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("hdhr: guide: %s", resp.Status)
	}
	var guide []GuideChannel
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&guide); err != nil {
		return nil, fmt.Errorf("hdhr: guide: %w", err)
	}
	return guide, nil
}

func (d *Device) getJSON(ctx context.Context, path string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.base+path, nil)
	if err != nil {
		return err
	}
	resp, err := d.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("hdhr: GET %s: %s", path, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

// Tuner is one tuner's status plus who is using it.
type Tuner struct {
	TunerStatus
	VChannel string // "13.1", or "none" when tuned by frequency or not at all
	Target   string // where it streams: "none", "udp://…", "http://10.10.0.6:55464"
	LockedBy string // IP that holds the tuner's lock, or "none"
}

// InUse reports whether something other than a plain tune holds the tuner:
// it is locked or streaming.
func (t Tuner) InUse() bool { return t.LockedBy != "none" || t.Target != "none" }

// Tuner reads tuner n.
func (d *Device) Tuner(ctx context.Context, n int) (Tuner, error) {
	p := tunerPath(n)
	var t Tuner
	debug, err := d.ctl.Get(ctx, p+"debug")
	if err != nil {
		return t, err
	}
	t.TunerStatus = ParseDebug(debug)
	if t.VChannel, err = d.ctl.Get(ctx, p+"vchannel"); err != nil {
		return t, err
	}
	if t.Target, err = d.ctl.Get(ctx, p+"target"); err != nil {
		return t, err
	}
	if t.LockedBy, err = d.ctl.Get(ctx, p+"lockkey"); err != nil {
		return t, err
	}
	t.VChannel, t.Target, t.LockedBy = orNone(t.VChannel), orNone(t.Target), orNone(t.LockedBy)
	return t, nil
}

// StreamInfo lists the programs on tuner n's current channel.
func (d *Device) StreamInfo(ctx context.Context, n int) ([]Program, error) {
	s, err := d.ctl.Get(ctx, tunerPath(n)+"streaminfo")
	if err != nil {
		return nil, err
	}
	return ParseStreamInfo(s), nil
}

// ErrLocked means another client holds the tuner's lock.
var ErrLocked = errors.New("hdhr: tuner is locked by another client")

// Lock takes tuner n's lock with key, which must be non-zero. It fails with
// ErrLocked when another client holds the lock; ForceUnlock breaks it.
func (d *Device) Lock(ctx context.Context, n int, key uint32) error {
	_, err := d.ctl.Set(ctx, tunerPath(n)+"lockkey", strconv.FormatUint(uint64(key), 10), 0)
	return lockedErr(err)
}

// Unlock releases a lock taken with key.
func (d *Device) Unlock(ctx context.Context, n int, key uint32) error {
	_, err := d.ctl.Set(ctx, tunerPath(n)+"lockkey", "none", key)
	return lockedErr(err)
}

// ForceUnlock breaks whatever lock tuner n has. Whoever held it (a DVR or a
// viewer) loses the tuner.
func (d *Device) ForceUnlock(ctx context.Context, n int) error {
	_, err := d.ctl.Set(ctx, tunerPath(n)+"lockkey", "force", 0)
	return err
}

// TuneVirtual tunes to a virtual channel from the lineup, such as "13.1".
func (d *Device) TuneVirtual(ctx context.Context, n int, vchannel string, key uint32) error {
	_, err := d.ctl.Set(ctx, tunerPath(n)+"vchannel", vchannel, key)
	return lockedErr(err)
}

// TuneChannel tunes to a physical channel: "auto:28" for an RF channel or
// "auto:557000000" for a frequency in Hz.
func (d *Device) TuneChannel(ctx context.Context, n int, channel string, key uint32) error {
	_, err := d.ctl.Set(ctx, tunerPath(n)+"channel", channel, key)
	return lockedErr(err)
}

// Untune stops the tuner (channel none), leaving it idle.
func (d *Device) Untune(ctx context.Context, n int, key uint32) error {
	_, err := d.ctl.Set(ctx, tunerPath(n)+"channel", "none", key)
	return lockedErr(err)
}

// SetTarget sets where tuner n streams; "none" stops the stream.
func (d *Device) SetTarget(ctx context.Context, n int, target string, key uint32) (string, error) {
	v, err := d.ctl.Set(ctx, tunerPath(n)+"target", target, key)
	return v, lockedErr(err)
}

// LockHolder returns the address that holds tuner n's lock, or "none".
func (d *Device) LockHolder(ctx context.Context, n int) (string, error) {
	v, err := d.ctl.Get(ctx, tunerPath(n)+"lockkey")
	return orNone(v), err
}

func tunerPath(n int) string { return "/tuner" + strconv.Itoa(n) + "/" }

func lockedErr(err error) error {
	var devErr *DeviceError
	if errors.As(err, &devErr) && isLockedMessage(devErr.Message) {
		return fmt.Errorf("%w (%s)", ErrLocked, devErr.Message)
	}
	return err
}

// The device answers sets on a tuner locked by someone else with
// "ERROR: resource locked by 10.10.0.6", and a set with a stale key with
// "ERROR: invalid lockkey".
func isLockedMessage(msg string) bool {
	return strings.Contains(strings.ToLower(msg), "lock")
}
