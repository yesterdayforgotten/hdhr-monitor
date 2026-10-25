package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"
)

// Network names and logos come from Silicondust's guide service, refreshed in
// the background twice a day and saved so a restart without internet still
// has them.

const (
	guideMaxAge     = 12 * time.Hour
	guideRetryAfter = 30 * time.Minute
)

type guideEntry struct {
	Affiliate string `json:"affiliate,omitempty"`
	Logo      string `json:"logo,omitempty"`
}

type guide struct {
	path string // saved copy; empty keeps it in memory

	mu      sync.Mutex
	loaded  bool
	at      time.Time // when the data was fetched
	tried   time.Time // last fetch attempt
	running bool
	entries map[string]guideEntry // by guide number, such as "13.1"
}

type savedGuide struct {
	At      time.Time             `json:"at"`
	Entries map[string]guideEntry `json:"entries"`
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func newGuide(dataDir, deviceID string) *guide {
	g := &guide{}
	if dataDir != "" {
		g.path = filepath.Join(dataDir, "guide-"+unsafeName.ReplaceAllString(deviceID, "_")+".json")
	}
	return g
}

// get returns what is known now and starts a refresh in the background when
// the data is old. It never waits on the network.
func (g *guide) get(d *device) map[string]guideEntry {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.loaded {
		g.loaded = true
		g.loadLocked()
	}
	if !d.cfg.Guide || g.running || time.Since(g.at) < guideMaxAge || time.Since(g.tried) < guideRetryAfter {
		return g.entries
	}
	g.running = true
	g.tried = time.Now()
	go g.refresh(d)
	return g.entries
}

func (g *guide) refresh(d *device) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	entries, err := fetchGuide(ctx, d)

	g.mu.Lock()
	defer g.mu.Unlock()
	g.running = false
	if err != nil {
		slog.Warn("couldn't fetch channel guide; network names and logos unavailable", "device", d.id, "err", err)
		return
	}
	g.entries, g.at = entries, time.Now()
	slog.Info("fetched channel guide", "device", d.id, "channels", len(entries))
	if err := g.saveLocked(); err != nil {
		slog.Warn("couldn't save channel guide", "path", g.path, "err", err)
	}
}

func fetchGuide(ctx context.Context, d *device) (map[string]guideEntry, error) {
	// The auth token changes over time, so read it fresh.
	info, err := d.dev.Info(ctx)
	if err != nil {
		return nil, err
	}
	if info.DeviceAuth == "" {
		return nil, errors.New("device reports no DeviceAuth")
	}
	channels, err := d.dev.Guide(ctx, info.DeviceAuth)
	if err != nil {
		return nil, err
	}
	entries := make(map[string]guideEntry, len(channels))
	for _, c := range channels {
		if c.Affiliate != "" || c.ImageURL != "" {
			entries[c.GuideNumber] = guideEntry{Affiliate: c.Affiliate, Logo: c.ImageURL}
		}
	}
	return entries, nil
}

func (g *guide) loadLocked() {
	if g.path == "" {
		return
	}
	data, err := os.ReadFile(g.path)
	if err != nil {
		return
	}
	var saved savedGuide
	if json.Unmarshal(data, &saved) == nil {
		g.at, g.entries = saved.At, saved.Entries
	}
}

func (g *guide) saveLocked() error {
	if g.path == "" {
		return nil
	}
	data, err := json.Marshal(savedGuide{g.at, g.entries})
	if err != nil {
		return err
	}
	tmp := g.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, g.path)
}
