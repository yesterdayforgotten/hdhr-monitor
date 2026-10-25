package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
	"time"

	"github.com/yesterdayforgotten/hdhr-monitor/internal/hdhrtest"
)

type harness struct {
	t    *testing.T
	fake *hdhrtest.Device
	srv  *Server
	id   string
}

func newHarness(t *testing.T, cfg Config) *harness {
	t.Helper()
	fake, err := hdhrtest.Start(2)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fake.Close)

	cfg.Hosts = []string{fake.Addr()}
	if cfg.LeaseTTL == 0 {
		cfg.LeaseTTL = time.Minute
	}
	cfg.Timeout = 2 * time.Second
	if cfg.TrustedNets == nil {
		cfg.TrustedNets = DefaultTrustedNets
	}
	cfg.Version = "test"
	web := fstest.MapFS{"index.html": {Data: []byte("<!doctype html>")}}
	srv, err := New(cfg, web)
	if err != nil {
		t.Fatal(err)
	}
	srv.devices[0].dev.SetHTTPBase(fake.HTTPBase())
	t.Cleanup(func() { srv.devices[0].dev.Close() })
	return &harness{t: t, fake: fake, srv: srv, id: fake.Addr()}
}

// do sends a request from a LAN address and decodes the JSON reply into out.
func (h *harness) do(method, path string, body, out any) int {
	h.t.Helper()
	var r *http.Request
	if body != nil {
		b, _ := json.Marshal(body)
		r = httptest.NewRequest(method, path, bytes.NewReader(b))
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	r.RemoteAddr = "10.10.0.20:5000"
	w := httptest.NewRecorder()
	h.srv.ServeHTTP(w, r)
	if out != nil {
		if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
			h.t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body)
		}
	}
	return w.Code
}

func (h *harness) api(rest string) string { return "/api/devices/" + h.id + rest }

func (h *harness) status(client string) statusJSON {
	h.t.Helper()
	h.srv.devices[0].invalidate()
	var st statusJSON
	if code := h.do("GET", h.api("/status?client="+client), nil, &st); code != http.StatusOK {
		h.t.Fatalf("status: %d", code)
	}
	return st
}

func TestDevicesAndStatus(t *testing.T) {
	h := newHarness(t, Config{})
	h.fake.Update(0, func(t *hdhrtest.Tuner) { t.Strength, t.SNQ, t.SymbolQuality = 94, 83, 100 })

	var devs []deviceJSON
	h.do("GET", "/api/devices", nil, &devs)
	if len(devs) != 1 || !devs[0].Online || devs[0].TunerCount != 2 || devs[0].Model != "HDFX-2US" {
		t.Fatalf("devices = %+v", devs)
	}

	st := h.status("a")
	if !st.Online || len(st.Tuners) != 2 {
		t.Fatalf("status = %+v", st)
	}
	if st.Tuners[0].State != "free" || st.Tuners[0].Strength != 94 || st.Tuners[0].SNQ != 83 {
		t.Errorf("tuner 0 = %+v", st.Tuners[0])
	}
}

func TestTuneOwnershipAndRelease(t *testing.T) {
	h := newHarness(t, Config{})

	var st statusJSON
	if code := h.do("POST", h.api("/tuners/1/tune"), TuneRequest{Client: "a", VChannel: "13.1"}, &st); code != http.StatusOK {
		t.Fatalf("tune: %d", code)
	}
	if got := h.fake.Tuner(1); got.VChannel != "13.1" || got.LockKey == 0 {
		t.Errorf("device tuner 1 = %+v", got)
	}
	tu := st.Tuners[1]
	if tu.State != "yours" || tu.VChannel != "13.1" || tu.Name != "KSTU-HD" || tu.RF != 28 {
		t.Errorf("tuner 1 for its owner = %+v", tu)
	}
	if got := h.status("b").Tuners[1].State; got != "app" {
		t.Errorf("tuner 1 for another viewer = %q, want app", got)
	}

	// Another viewer needs to force it.
	var conflict map[string]any
	if code := h.do("POST", h.api("/tuners/1/tune"), TuneRequest{Client: "b", VChannel: "2.1"}, &conflict); code != http.StatusConflict {
		t.Fatalf("tune by another viewer: %d %v", code, conflict)
	}
	if code := h.do("POST", h.api("/tuners/1/tune"), TuneRequest{Client: "b", VChannel: "2.1", Force: true}, nil); code != http.StatusOK {
		t.Fatalf("forced tune: %d", code)
	}
	if got := h.status("b").Tuners[1].State; got != "yours" {
		t.Errorf("after takeover = %q, want yours", got)
	}

	// Only the owner can release.
	h.do("POST", h.api("/tuners/1/release"), releaseRequest{Client: "a"}, nil)
	if h.fake.Tuner(1).LockKey == 0 {
		t.Error("a viewer that lost the tuner released it")
	}
	h.do("POST", h.api("/tuners/1/release"), releaseRequest{Client: "b"}, nil)
	if got := h.fake.Tuner(1); got.LockKey != 0 || got.Channel != "none" {
		t.Errorf("after release, device tuner 1 = %+v", got)
	}
	if got := h.status("b").Tuners[1].State; got != "free" {
		t.Errorf("after release = %q, want free", got)
	}
}

func TestTuneByRFLearnsPrograms(t *testing.T) {
	h := newHarness(t, Config{})

	if code := h.do("POST", h.api("/tuners/0/tune"), TuneRequest{Client: "a", RF: 28}, nil); code != http.StatusOK {
		t.Fatalf("tune: %d", code)
	}
	// The first status read learns the station from streaminfo.
	h.status("a")
	tu := h.status("a").Tuners[0]
	if tu.RF != 28 || tu.VChannel != "13.1" || tu.Name != "KSTU-HD" {
		t.Errorf("tuner 0 = %+v", tu)
	}

	var lineup []channelJSON
	h.do("GET", h.api("/lineup"), nil, &lineup)
	rf := map[string]int{}
	for _, c := range lineup {
		rf[c.Number] = c.RF
	}
	if rf["13.1"] != 28 || rf["13.2"] != 28 || rf["2.1"] != 0 {
		t.Errorf("lineup RF = %v", rf)
	}
}

func TestTunerInUseElsewhere(t *testing.T) {
	h := newHarness(t, Config{})
	// A viewer is streaming from tuner 0 with its own lock.
	h.fake.Update(0, func(t *hdhrtest.Tuner) {
		t.Channel, t.VChannel, t.Target = "auto:593000000", "2.1", "http://10.10.0.6:55464"
		t.LockKey, t.LockHolder = 77, "10.10.0.6"
	})

	if got := h.status("a").Tuners[0]; got.State != "in_use" || got.LockedBy != "10.10.0.6" {
		t.Errorf("tuner 0 = %+v", got)
	}

	var conflict struct {
		InUse InUseError `json:"inUse"`
	}
	if code := h.do("POST", h.api("/tuners/0/tune"), TuneRequest{Client: "a", VChannel: "13.1"}, &conflict); code != http.StatusConflict {
		t.Fatalf("tune: %d", code)
	}
	if conflict.InUse.LockedBy != "10.10.0.6" || conflict.InUse.ByApp {
		t.Errorf("conflict = %+v", conflict.InUse)
	}
	if h.fake.Tuner(0).VChannel != "2.1" {
		t.Error("an unforced tune changed the channel")
	}

	if code := h.do("POST", h.api("/tuners/0/tune"), TuneRequest{Client: "a", VChannel: "13.1", Force: true}, nil); code != http.StatusOK {
		t.Fatalf("forced tune: %d", code)
	}
	if got := h.fake.Tuner(0); got.VChannel != "13.1" || got.Target != "none" || got.LockHolder != "127.0.0.1" {
		t.Errorf("after force, device tuner 0 = %+v", got)
	}
}

func TestLostLockIsNoticed(t *testing.T) {
	h := newHarness(t, Config{})
	h.do("POST", h.api("/tuners/0/tune"), TuneRequest{Client: "a", VChannel: "13.1"}, nil)

	// Another app forces the lock away.
	h.fake.Update(0, func(t *hdhrtest.Tuner) { t.LockKey, t.LockHolder = 5, "10.10.0.6" })
	if got := h.status("a").Tuners[0].State; got != "in_use" {
		t.Errorf("after losing the lock = %q, want in_use", got)
	}
}

func TestIdleLeasesExpire(t *testing.T) {
	h := newHarness(t, Config{LeaseTTL: 50 * time.Millisecond})
	h.do("POST", h.api("/tuners/0/tune"), TuneRequest{Client: "a", VChannel: "13.1"}, nil)

	time.Sleep(100 * time.Millisecond)
	h.srv.devices[0].expire(context.Background())
	if got := h.fake.Tuner(0); got.LockKey != 0 || got.Channel != "none" {
		t.Errorf("after the lease ran out, device tuner 0 = %+v", got)
	}
}

func TestPollingRenewsLease(t *testing.T) {
	h := newHarness(t, Config{LeaseTTL: 150 * time.Millisecond})
	h.do("POST", h.api("/tuners/0/tune"), TuneRequest{Client: "a", VChannel: "13.1"}, nil)

	for range 4 {
		time.Sleep(60 * time.Millisecond)
		h.status("a")
		h.srv.devices[0].expire(context.Background())
	}
	if h.fake.Tuner(0).LockKey == 0 {
		t.Error("a polling viewer lost its tuner")
	}
}

func TestBadTuneRequests(t *testing.T) {
	h := newHarness(t, Config{})
	for _, req := range []TuneRequest{
		{VChannel: "13.1"},                      // no client
		{Client: "a"},                           // nothing to tune to
		{Client: "a", VChannel: "13.1", RF: 28}, // two targets
		{Client: "a", RF: 99},
		{Client: "a", Frequency: 1234},
	} {
		if code := h.do("POST", h.api("/tuners/0/tune"), req, nil); code != http.StatusBadRequest {
			t.Errorf("tune %+v: %d, want 400", req, code)
		}
	}
	if code := h.do("POST", h.api("/tuners/5/tune"), TuneRequest{Client: "a", VChannel: "13.1"}, nil); code != http.StatusBadRequest {
		t.Errorf("tune tuner 5: %d, want 400", code)
	}
	if code := h.do("GET", "/api/devices/nope/status", nil, nil); code != http.StatusNotFound {
		t.Errorf("unknown device: %d, want 404", code)
	}
}

func TestReleaseAllOnShutdown(t *testing.T) {
	h := newHarness(t, Config{})
	h.do("POST", h.api("/tuners/0/tune"), TuneRequest{Client: "a", VChannel: "13.1"}, nil)
	h.do("POST", h.api("/tuners/1/tune"), TuneRequest{Client: "b", RF: 28}, nil)

	h.srv.devices[0].releaseAll(context.Background())
	for n := range 2 {
		if got := h.fake.Tuner(n); got.LockKey != 0 || got.Channel != "none" {
			t.Errorf("device tuner %d = %+v", n, got)
		}
	}
}

func TestFavorites(t *testing.T) {
	dir := t.TempDir()
	h := newHarness(t, Config{DataDir: dir})

	favs := []Favorite{{VChannel: "13.1", Label: "FOX"}, {RF: 28}}
	var got []Favorite
	if code := h.do("PUT", h.api("/favorites"), favs, &got); code != http.StatusOK || len(got) != 2 {
		t.Fatalf("put: %d %v", code, got)
	}

	// Saved to disk, so a restart keeps them.
	again, err := loadFavorites(dir)
	if err != nil {
		t.Fatal(err)
	}
	if saved := again.get(h.id); len(saved) != 2 || saved[0].Label != "FOX" || saved[1].RF != 28 {
		t.Errorf("reloaded = %v", saved)
	}
	if _, err := os.Stat(filepath.Join(dir, "favorites.json.tmp")); err == nil {
		t.Error("temporary file left behind")
	}

	for _, bad := range [][]Favorite{
		{{}},
		{{VChannel: "13.1", RF: 28}},
		{{RF: 1}},
		{{VChannel: "13.1"}, {VChannel: " 13.1 "}},
	} {
		if code := h.do("PUT", h.api("/favorites"), bad, nil); code != http.StatusBadRequest {
			t.Errorf("put %v: %d, want 400", bad, code)
		}
	}

	h.do("PUT", h.api("/favorites"), []Favorite{}, nil)
	h.do("GET", h.api("/favorites"), nil, &got)
	if len(got) != 0 {
		t.Errorf("after clearing = %v", got)
	}
}

func TestAuth(t *testing.T) {
	get := func(srv *Server, remote string, header http.Header, user, pass string) int {
		r := httptest.NewRequest("GET", "/api/version", nil)
		r.RemoteAddr = remote
		for k, v := range header {
			r.Header[k] = v
		}
		if user != "" {
			r.SetBasicAuth(user, pass)
		}
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, r)
		return w.Code
	}
	proxied := http.Header{"X-Forwarded-For": {"203.0.113.9"}}

	open := newHarness(t, Config{}).srv
	for _, tc := range []struct {
		name   string
		remote string
		header http.Header
		want   int
	}{
		{"LAN", "10.10.0.20:5000", nil, http.StatusOK},
		{"loopback", "127.0.0.1:5000", nil, http.StatusOK},
		{"IPv4-mapped LAN", "[::ffff:192.168.1.5]:5000", nil, http.StatusOK},
		{"internet", "203.0.113.9:5000", nil, http.StatusForbidden},
		{"through a proxy", "10.10.0.1:5000", proxied, http.StatusForbidden},
	} {
		if got := get(open, tc.remote, tc.header, "", ""); got != tc.want {
			t.Errorf("no password, %s: %d, want %d", tc.name, got, tc.want)
		}
	}

	locked := newHarness(t, Config{Username: "admin", Password: "secret"}).srv
	if got := get(locked, "10.10.0.1:5000", proxied, "", ""); got != http.StatusUnauthorized {
		t.Errorf("proxied, no login: %d, want 401", got)
	}
	if got := get(locked, "10.10.0.1:5000", proxied, "admin", "wrong"); got != http.StatusUnauthorized {
		t.Errorf("proxied, wrong password: %d, want 401", got)
	}
	if got := get(locked, "10.10.0.1:5000", proxied, "admin", "secret"); got != http.StatusOK {
		t.Errorf("proxied, right password: %d, want 200", got)
	}

	// Health checks never need a login.
	r := httptest.NewRequest("GET", "/healthz", nil)
	r.RemoteAddr = "203.0.113.9:5000"
	w := httptest.NewRecorder()
	locked.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("healthz from outside: %d", w.Code)
	}
}

func TestOfflineDevice(t *testing.T) {
	h := newHarness(t, Config{})
	h.fake.Close()

	st := h.status("a")
	if st.Online || st.Error == "" {
		t.Errorf("status of a stopped device = %+v", st)
	}
}

func TestStaticFiles(t *testing.T) {
	h := newHarness(t, Config{})
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.10.0.20:5000"
	w := httptest.NewRecorder()
	h.srv.ServeHTTP(w, r)
	if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "no-cache" {
		t.Errorf("GET /: %d, Cache-Control %q", w.Code, w.Header().Get("Cache-Control"))
	}
	if code := h.do("GET", "/api/nope", nil, nil); code != http.StatusNotFound {
		t.Errorf("unknown API path: %d, want 404", code)
	}
}

func TestTrustedProxy(t *testing.T) {
	srv := newHarness(t, Config{TrustedProxies: []netip.Prefix{netip.MustParsePrefix("10.10.0.2/32")}}).srv
	get := func(remote string, xff ...string) int {
		r := httptest.NewRequest("GET", "/api/version", nil)
		r.RemoteAddr = remote
		for _, v := range xff {
			r.Header.Add("X-Forwarded-For", v)
		}
		r.Header.Set("X-Forwarded-Host", "hdhr.example.com")
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, r)
		return w.Code
	}

	for _, tc := range []struct {
		name   string
		remote string
		xff    []string
		want   int
	}{
		{"LAN phone through the proxy", "10.10.0.2:40000", []string{"10.10.0.20"}, http.StatusOK},
		{"internet through the proxy", "10.10.0.2:40000", []string{"203.0.113.9"}, http.StatusForbidden},
		{"spoofed LAN address", "10.10.0.2:40000", []string{"10.10.0.20, 203.0.113.9"}, http.StatusForbidden},
		{"spoofed header line", "10.10.0.2:40000", []string{"10.10.0.20", "203.0.113.9"}, http.StatusForbidden},
		{"proxy without a client address", "10.10.0.2:40000", nil, http.StatusForbidden},
		{"some other proxy", "10.10.0.9:40000", []string{"10.10.0.20"}, http.StatusForbidden},
	} {
		if got := get(tc.remote, tc.xff...); got != tc.want {
			t.Errorf("%s: %d, want %d", tc.name, got, tc.want)
		}
	}
}
