// Package server is the HTTP API and web page for monitoring HDHomeRun
// tuners. Browsers poll it; it polls the devices only while asked.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/yesterdayforgotten/hdhr-monitor/internal/hdhr"
)

type Config struct {
	Hosts []string

	// CacheTTL is how long one status read serves every browser.
	CacheTTL time.Duration
	// LeaseTTL is how long a tuner stays taken after its browser stops
	// polling (closed tab, phone asleep).
	LeaseTTL time.Duration
	// Timeout bounds each request to a device.
	Timeout time.Duration

	// Requests from these networks that didn't come through a proxy need no
	// login. Everything else needs Username and Password, and is refused
	// when no password is set.
	TrustedNets []netip.Prefix
	// NoAuth lets every request in, for when something in front (a LAN-only
	// reverse proxy, a firewall) already decides who may connect.
	NoAuth bool
	// TrustedProxies are reverse proxies whose X-Forwarded-For is believed:
	// a request through one is judged by the client address it reports, so
	// LAN users of a LAN-only proxy need no login.
	TrustedProxies []netip.Prefix
	Username       string
	Password       string

	// DataDir holds favorites.json. Empty keeps favorites in memory only.
	DataDir string

	// Guide fetches network names and station logos from Silicondust's
	// guide service, as the HDHomeRun apps do.
	Guide bool

	// Measure has ATSC 3.0 tuners held for a page stream their station to
	// this server, which discards it, because that is the only time they
	// report its bitrate. It costs that bitrate (several Mbps) of network
	// traffic per tuner. StreamPort is the UDP port the streams arrive on,
	// and StreamHost the address devices should send them to; empty works it
	// out from the route to each device.
	Measure    bool
	StreamPort int
	StreamHost string

	// Version identifies the running build. Open pages reload when it
	// changes.
	Version string
}

type Server struct {
	cfg     Config
	devices []*device
	byID    map[string]*device
	favs    *favorites
	sink    *sink // nil unless cfg.Measure
	mux     *http.ServeMux
}

func New(cfg Config, web fs.FS) (*Server, error) {
	favs, err := loadFavorites(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	s := &Server{cfg: cfg, byID: map[string]*device{}, favs: favs, mux: http.NewServeMux()}
	if cfg.Measure {
		if s.sink, err = listenSink(cfg.StreamPort); err != nil {
			return nil, err
		}
	}
	for _, h := range cfg.Hosts {
		if _, dup := s.byID[h]; dup {
			continue
		}
		d := newDevice(h, &s.cfg)
		d.sink = s.sink
		s.devices = append(s.devices, d)
		s.byID[d.id] = d
	}

	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok\n") })
	s.mux.HandleFunc("GET /api/version", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"version": s.cfg.Version})
	})
	s.mux.HandleFunc("GET /api/devices", s.handleDevices)
	s.mux.HandleFunc("GET /api/devices/{id}/lineup", s.handleLineup)
	s.mux.HandleFunc("GET /api/devices/{id}/status", s.handleStatus)
	s.mux.HandleFunc("POST /api/devices/{id}/tuners/{n}/tune", s.handleTune)
	s.mux.HandleFunc("POST /api/devices/{id}/tuners/{n}/release", s.handleRelease)
	s.mux.HandleFunc("GET /api/devices/{id}/favorites", s.handleGetFavorites)
	s.mux.HandleFunc("PUT /api/devices/{id}/favorites", s.handlePutFavorites)
	s.mux.Handle("GET /", staticHandler(web))
	return s, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/healthz" && !s.authorized(w, r) {
		return
	}
	s.mux.ServeHTTP(w, r)
}

// Run releases idle tuners until ctx ends, then releases every tuner it
// holds.
func (s *Server) Run(ctx context.Context) {
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			done, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			for _, d := range s.devices {
				d.releaseAll(done)
				d.dev.Close()
			}
			if s.sink != nil {
				s.sink.close()
			}
			return
		case <-tick.C:
			for _, d := range s.devices {
				ectx, cancel := context.WithTimeout(ctx, 5*time.Second)
				d.expire(ectx)
				cancel()
			}
		}
	}
}

type deviceJSON struct {
	ID         string `json:"id"`
	Host       string `json:"host"`
	Online     bool   `json:"online"`
	Name       string `json:"name,omitempty"`
	Model      string `json:"model,omitempty"`
	Firmware   string `json:"firmware,omitempty"`
	DeviceID   string `json:"deviceId,omitempty"`
	TunerCount int    `json:"tunerCount"`
}

func (s *Server) handleDevices(w http.ResponseWriter, r *http.Request) {
	out := make([]deviceJSON, len(s.devices))
	for i, d := range s.devices {
		out[i] = deviceJSON{ID: d.id, Host: d.dev.Host}
		if info, err := d.getInfo(r.Context()); err == nil {
			out[i].Online = true
			out[i].Name = info.FriendlyName
			out[i].Model = info.ModelNumber
			out[i].Firmware = info.FirmwareVersion
			out[i].DeviceID = info.DeviceID
			out[i].TunerCount = info.TunerCount
		}
	}
	writeJSON(w, http.StatusOK, out)
}

type channelJSON struct {
	Number string `json:"number"`
	Name   string `json:"name"`
	HD     bool   `json:"hd,omitempty"`
	DRM    bool   `json:"drm,omitempty"`
	// RF is the physical channel, known once some tuner has been on it.
	RF int `json:"rf,omitempty"`
	// Affiliate is the network, such as "FOX", and Logo a URL of the
	// station's logo (white on transparent), both from the guide service.
	Affiliate string `json:"affiliate,omitempty"`
	Logo      string `json:"logo,omitempty"`
}

func (s *Server) handleLineup(w http.ResponseWriter, r *http.Request) {
	d := s.device(w, r)
	if d == nil {
		return
	}
	lineup, err := d.getLineup(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	rf := map[string]int{}
	d.mu.Lock()
	for freq, progs := range d.programs {
		for _, p := range progs {
			rf[p.VChannel] = hdhr.RFChannel(freq)
		}
	}
	d.mu.Unlock()
	guide := d.guide.get(d)
	out := make([]channelJSON, len(lineup))
	for i, c := range lineup {
		g := guide[c.GuideNumber]
		out[i] = channelJSON{
			Number:    c.GuideNumber,
			Name:      c.GuideName,
			HD:        c.HD != 0,
			DRM:       c.DRM != 0,
			RF:        rf[c.GuideNumber],
			Affiliate: g.Affiliate,
			Logo:      g.Logo,
		}
	}
	writeJSON(w, http.StatusOK, out)
}

type tunerJSON struct {
	Index     int    `json:"index"`
	Channel   string `json:"channel"`
	Frequency int64  `json:"frequency"`
	RF        int    `json:"rf,omitempty"`
	VChannel  string `json:"vchannel,omitempty"`
	Name      string `json:"name,omitempty"`

	Lock          string `json:"lock"`
	Locked        bool   `json:"locked"`
	Strength      int    `json:"strength"`
	SNQ           int    `json:"snq"`
	SymbolQuality int    `json:"symbolQuality"`

	StreamBPS       int64 `json:"streamBps"`
	DeviceBPS       int64 `json:"deviceBps"`
	TransportErrors int64 `json:"transportErrors"`
	CRCErrors       int64 `json:"crcErrors"`

	Target   string `json:"target"`
	LockedBy string `json:"lockedBy"`
	// State is "free", "yours" (this browser holds it), "app" (another
	// browser of this app holds it) or "in_use" (something else is using it).
	State string `json:"state"`
}

type statusJSON struct {
	Time   int64       `json:"time"` // ms since the epoch, when the device was read
	Online bool        `json:"online"`
	Error  string      `json:"error,omitempty"`
	Tuners []tunerJSON `json:"tuners"`
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	d := s.device(w, r)
	if d == nil {
		return
	}
	client := r.URL.Query().Get("client")
	d.renew(client)
	writeJSON(w, http.StatusOK, s.status(r.Context(), d, client))
}

func (s *Server) status(ctx context.Context, d *device, client string) statusJSON {
	snap := d.getSnapshot(ctx)
	out := statusJSON{Time: snap.at.UnixMilli(), Online: snap.err == nil, Tuners: []tunerJSON{}}
	if snap.err != nil {
		out.Error = snap.err.Error()
		return out
	}

	names := map[string]string{}
	if lineup, err := d.getLineup(ctx); err == nil {
		for _, c := range lineup {
			names[c.GuideNumber] = c.GuideName
		}
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	for i, t := range snap.tuners {
		tj := tunerJSON{
			Index:           i,
			Channel:         t.Channel,
			Frequency:       t.Frequency,
			RF:              hdhr.RFChannel(t.Frequency),
			Lock:            t.Lock,
			Locked:          t.Locked,
			Strength:        t.Strength,
			SNQ:             t.SNQ,
			SymbolQuality:   t.SymbolQuality,
			StreamBPS:       t.StreamBPS,
			DeviceBPS:       t.DeviceBPS,
			TransportErrors: t.TransportErrors,
			CRCErrors:       t.CRCErrors,
			Target:          t.Target,
			LockedBy:        t.LockedBy,
			State:           "free",
		}
		if t.Lock == "atsc3" {
			// 3.0 tuners leave ts: at zero; what they send is the station.
			tj.StreamBPS = t.NetBPS
		}
		if t.VChannel != "none" {
			tj.VChannel, tj.Name = t.VChannel, names[t.VChannel]
		} else if progs := d.programs[t.Frequency]; len(progs) > 0 {
			tj.VChannel, tj.Name = progs[0].VChannel, progs[0].Name
		}
		switch o := d.owners[i]; {
		case o != nil && o.client == client:
			tj.State = "yours"
		case o != nil:
			tj.State = "app"
		case t.InUse():
			tj.State = "in_use"
		}
		out.Tuners = append(out.Tuners, tj)
	}
	return out
}

func (s *Server) handleTune(w http.ResponseWriter, r *http.Request) {
	d := s.device(w, r)
	if d == nil {
		return
	}
	n, ok := tunerIndex(w, r)
	if !ok {
		return
	}
	var req TuneRequest
	if !readJSON(w, r, &req) {
		return
	}

	err := d.tune(r.Context(), n, req)
	var inUse *InUseError
	switch {
	case errors.As(err, &inUse):
		writeJSON(w, http.StatusConflict, map[string]any{"error": inUse.Error(), "inUse": inUse})
	case errors.Is(err, errBadRequest):
		writeError(w, http.StatusBadRequest, err)
	case errors.Is(err, hdhr.ErrLocked):
		writeError(w, http.StatusConflict, err)
	case err != nil:
		slog.Warn("tune failed", "device", d.id, "tuner", n, "err", err)
		writeError(w, http.StatusBadGateway, err)
	default:
		slog.Info("tuned", "device", d.id, "tuner", n, "client", req.Client,
			"vchannel", req.VChannel, "rf", req.RF, "frequency", req.Frequency, "force", req.Force)
		writeJSON(w, http.StatusOK, s.status(r.Context(), d, req.Client))
	}
}

type releaseRequest struct {
	Client string `json:"client"`
}

func (s *Server) handleRelease(w http.ResponseWriter, r *http.Request) {
	d := s.device(w, r)
	if d == nil {
		return
	}
	n, ok := tunerIndex(w, r)
	if !ok {
		return
	}
	// Browsers send this with navigator.sendBeacon as the page closes, which
	// can't set a JSON content type, so accept any.
	var req releaseRequest
	if !readJSON(w, r, &req) {
		return
	}
	if err := d.release(r.Context(), n, req.Client); err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, s.status(r.Context(), d, req.Client))
}

func (s *Server) device(w http.ResponseWriter, r *http.Request) *device {
	d := s.byID[r.PathValue("id")]
	if d == nil {
		writeError(w, http.StatusNotFound, errors.New("unknown device"))
	}
	return d
}

func tunerIndex(w http.ResponseWriter, r *http.Request) (int, bool) {
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil || n < 0 {
		writeError(w, http.StatusBadRequest, errors.New("bad tuner number"))
		return 0, false
	}
	return n, true
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, errors.New("bad JSON body"))
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func staticHandler(web fs.FS) http.Handler {
	files := http.FileServerFS(web)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			writeError(w, http.StatusNotFound, errors.New("not found"))
			return
		}
		// The page is small and changes with every release; always revalidate.
		w.Header().Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	})
}
