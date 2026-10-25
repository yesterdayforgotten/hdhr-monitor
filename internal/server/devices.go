package server

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/yesterdayforgotten/hdhr-monitor/internal/hdhr"
)

// device is one configured HDHomeRun and what the server knows about it.
type device struct {
	id  string
	dev *hdhr.Device
	cfg *Config

	// fetchMu serializes status reads, so that many browsers polling at once
	// cost the device one read per cache period.
	fetchMu sync.Mutex
	// opMu serializes lock, tune and release, which read and then change a
	// tuner.
	opMu sync.Mutex

	mu       sync.Mutex // guards everything below
	info     *hdhr.Info
	infoAt   time.Time
	lineup   []hdhr.LineupEntry
	lineupAt time.Time
	snap     *snapshot
	snapAt   time.Time
	owners   map[int]*owner
	selfIP   string                   // our address as the device reports lock holders
	programs map[int64][]hdhr.Program // streaminfo by frequency
	progAt   map[int64]time.Time

	guide *guide // network names and logos
}

// owner is a browser that has taken a tuner. The server holds the device lock
// for it until it releases the tuner or stops polling for LeaseTTL.
type owner struct {
	client string
	key    uint32
	seen   time.Time
}

type snapshot struct {
	at     time.Time
	err    error
	tuners []hdhr.Tuner
}

func newDevice(host string, cfg *Config) *device {
	return &device{
		id:       host,
		dev:      hdhr.NewDevice(host, cfg.Timeout),
		cfg:      cfg,
		owners:   map[int]*owner{},
		programs: map[int64][]hdhr.Program{},
		progAt:   map[int64]time.Time{},
		guide:    newGuide(cfg.DataDir, host),
	}
}

// getInfo returns discover.json, cached for a minute (ten seconds after a
// failure).
func (d *device) getInfo(ctx context.Context) (*hdhr.Info, error) {
	d.mu.Lock()
	info, at := d.info, d.infoAt
	d.mu.Unlock()
	if info != nil && time.Since(at) < time.Minute {
		return info, nil
	}
	if info == nil && time.Since(at) < 10*time.Second {
		return nil, errors.New("device unreachable")
	}

	fresh, err := d.dev.Info(ctx)
	d.mu.Lock()
	defer d.mu.Unlock()
	d.infoAt = time.Now()
	if err != nil {
		// Keep a known device's last info; its tuner count doesn't change.
		if d.info != nil {
			return d.info, nil
		}
		return nil, err
	}
	d.info = &fresh
	return d.info, nil
}

// getLineup returns lineup.json, cached for ten minutes.
func (d *device) getLineup(ctx context.Context) ([]hdhr.LineupEntry, error) {
	d.mu.Lock()
	lineup, at := d.lineup, d.lineupAt
	d.mu.Unlock()
	if lineup != nil && time.Since(at) < 10*time.Minute {
		return lineup, nil
	}

	fresh, err := d.dev.Lineup(ctx)
	if err != nil {
		if lineup != nil {
			return lineup, nil
		}
		return nil, err
	}
	d.mu.Lock()
	d.lineup, d.lineupAt = fresh, time.Now()
	d.mu.Unlock()
	return fresh, nil
}

func (d *device) tunerCount(ctx context.Context) (int, error) {
	info, err := d.getInfo(ctx)
	if err != nil {
		return 0, err
	}
	return info.TunerCount, nil
}

// getSnapshot returns every tuner's status, read at most once per CacheTTL.
func (d *device) getSnapshot(ctx context.Context) *snapshot {
	d.fetchMu.Lock()
	defer d.fetchMu.Unlock()

	d.mu.Lock()
	snap, at := d.snap, d.snapAt
	d.mu.Unlock()
	ttl := d.cfg.CacheTTL
	if snap != nil && snap.err != nil {
		ttl = 2 * time.Second // don't hammer a device that isn't answering
	}
	if snap != nil && time.Since(at) < ttl {
		return snap
	}

	snap = d.readSnapshot(ctx)
	d.mu.Lock()
	d.snap, d.snapAt = snap, time.Now()
	d.checkOwnersLocked(snap)
	d.mu.Unlock()
	if snap.err == nil {
		d.learnPrograms(ctx, snap)
	}
	return snap
}

func (d *device) readSnapshot(ctx context.Context) *snapshot {
	snap := &snapshot{at: time.Now()}
	n, err := d.tunerCount(ctx)
	if err != nil {
		snap.err = err
		return snap
	}
	for i := range n {
		t, err := d.dev.Tuner(ctx, i)
		if err != nil {
			snap.err = err
			snap.tuners = nil
			return snap
		}
		snap.tuners = append(snap.tuners, t)
	}
	return snap
}

func (d *device) invalidate() {
	d.mu.Lock()
	d.snap = nil
	d.mu.Unlock()
}

// checkOwnersLocked drops ownership of tuners whose lock we no longer hold:
// someone forced it, or the device restarted.
func (d *device) checkOwnersLocked(snap *snapshot) {
	if snap.err != nil {
		return
	}
	for n, o := range d.owners {
		if n >= len(snap.tuners) {
			delete(d.owners, n)
			continue
		}
		lockedBy := snap.tuners[n].LockedBy
		if lockedBy == "none" || (d.selfIP != "" && lockedBy != d.selfIP) {
			slog.Info("lost tuner lock", "device", d.id, "tuner", n, "client", o.client, "locked_by", lockedBy)
			delete(d.owners, n)
		}
	}
}

// learnPrograms reads streaminfo for frequencies we haven't seen, so tuners
// tuned by RF channel can still show the station.
func (d *device) learnPrograms(ctx context.Context, snap *snapshot) {
	for i, t := range snap.tuners {
		if !t.Locked || t.Frequency == 0 {
			continue
		}
		d.mu.Lock()
		progs, at := d.programs[t.Frequency], d.progAt[t.Frequency]
		d.mu.Unlock()
		if len(progs) > 0 || time.Since(at) < 5*time.Second {
			continue
		}
		progs, err := d.dev.StreamInfo(ctx, i)
		d.mu.Lock()
		d.progAt[t.Frequency] = time.Now()
		if err == nil {
			d.programs[t.Frequency] = progs
		}
		d.mu.Unlock()
	}
}

// renew extends the leases of the tuners client owns.
func (d *device) renew(client string) {
	if client == "" {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, o := range d.owners {
		if o.client == client {
			o.seen = time.Now()
		}
	}
}

// expire releases tuners whose owners stopped polling.
func (d *device) expire(ctx context.Context) {
	d.mu.Lock()
	var stale []int
	for n, o := range d.owners {
		if time.Since(o.seen) > d.cfg.LeaseTTL {
			stale = append(stale, n)
		}
	}
	d.mu.Unlock()

	for _, n := range stale {
		d.opMu.Lock()
		d.mu.Lock()
		o := d.owners[n]
		expired := o != nil && time.Since(o.seen) > d.cfg.LeaseTTL
		d.mu.Unlock()
		if expired {
			slog.Info("releasing idle tuner", "device", d.id, "tuner", n, "client", o.client)
			d.releaseLocked(ctx, n, o)
		}
		d.opMu.Unlock()
	}
}

// TuneRequest is the body of a tune call. Exactly one of VChannel, RF and
// Frequency is set.
type TuneRequest struct {
	Client    string `json:"client"`
	VChannel  string `json:"vchannel,omitempty"`
	RF        int    `json:"rf,omitempty"`
	Frequency int64  `json:"frequency,omitempty"`
	// Force takes the tuner even though it is in use, cutting off whoever
	// is using it.
	Force bool `json:"force,omitempty"`
}

// InUseError means the tuner is busy and the request didn't force it.
type InUseError struct {
	LockedBy string `json:"lockedBy"`
	Target   string `json:"target"`
	ByApp    bool   `json:"byApp"` // another browser of this app holds it
}

func (e *InUseError) Error() string {
	if e.ByApp {
		return "tuner is in use by another viewer of this app"
	}
	return fmt.Sprintf("tuner is in use by %s", e.LockedBy)
}

var errBadRequest = errors.New("bad request")

func (d *device) tune(ctx context.Context, n int, req TuneRequest) error {
	var channel string
	switch {
	case req.Client == "":
		return fmt.Errorf("%w: client is required", errBadRequest)
	case req.VChannel != "" && req.RF == 0 && req.Frequency == 0:
	case req.RF > 0 && req.VChannel == "" && req.Frequency == 0:
		if req.RF < 2 || req.RF > 69 {
			return fmt.Errorf("%w: RF channel must be 2–69", errBadRequest)
		}
		channel = fmt.Sprintf("auto:%d", req.RF)
	case req.Frequency > 0 && req.VChannel == "" && req.RF == 0:
		if req.Frequency < 50_000_000 || req.Frequency > 1_000_000_000 {
			return fmt.Errorf("%w: frequency must be in Hz", errBadRequest)
		}
		channel = fmt.Sprintf("auto:%d", req.Frequency)
	default:
		return fmt.Errorf("%w: give one of vchannel, rf or frequency", errBadRequest)
	}

	count, err := d.tunerCount(ctx)
	if err != nil {
		return err
	}
	if n < 0 || n >= count {
		return fmt.Errorf("%w: no tuner %d", errBadRequest, n)
	}

	d.opMu.Lock()
	defer d.opMu.Unlock()
	defer d.invalidate()

	o, err := d.acquireLocked(ctx, n, req.Client, req.Force)
	if err != nil {
		return err
	}
	if req.VChannel != "" {
		err = d.dev.TuneVirtual(ctx, n, req.VChannel, o.key)
	} else {
		err = d.dev.TuneChannel(ctx, n, channel, o.key)
	}
	if errors.Is(err, hdhr.ErrLocked) {
		// Someone forced the lock away between our lock and tune.
		d.mu.Lock()
		delete(d.owners, n)
		d.mu.Unlock()
	}
	return err
}

// acquireLocked makes client the owner of tuner n, taking the device lock if
// we don't already hold it. d.opMu must be held.
func (d *device) acquireLocked(ctx context.Context, n int, client string, force bool) (*owner, error) {
	d.mu.Lock()
	o := d.owners[n]
	d.mu.Unlock()

	if o != nil {
		if o.client != client {
			if !force {
				return nil, &InUseError{ByApp: true}
			}
			slog.Info("tuner taken over from another viewer", "device", d.id, "tuner", n, "from", o.client, "to", client)
		}
		d.mu.Lock()
		o.client, o.seen = client, time.Now()
		d.mu.Unlock()
		return o, nil
	}

	t, err := d.dev.Tuner(ctx, n)
	if err != nil {
		return nil, err
	}
	if t.InUse() {
		if !force {
			return nil, &InUseError{LockedBy: t.LockedBy, Target: t.Target}
		}
		slog.Info("forcing tuner", "device", d.id, "tuner", n, "locked_by", t.LockedBy, "target", t.Target)
		if t.LockedBy != "none" {
			if err := d.dev.ForceUnlock(ctx, n); err != nil {
				return nil, err
			}
		}
	}

	key := newLockKey()
	if err := d.dev.Lock(ctx, n, key); err != nil {
		if errors.Is(err, hdhr.ErrLocked) {
			return nil, &InUseError{LockedBy: "another client"}
		}
		return nil, err
	}
	if t.Target != "none" {
		// A stream started without a lock; stop sending it to its viewer.
		if _, err := d.dev.SetTarget(ctx, n, "none", key); err != nil {
			slog.Warn("couldn't clear stream target", "device", d.id, "tuner", n, "err", err)
		}
	}

	o = &owner{client: client, key: key, seen: time.Now()}
	d.mu.Lock()
	d.owners[n] = o
	d.mu.Unlock()

	// Learn how the device names us as a lock holder, to notice when someone
	// takes the tuner away.
	if self, err := d.dev.LockHolder(ctx, n); err == nil && self != "none" {
		d.mu.Lock()
		d.selfIP = self
		d.mu.Unlock()
	}
	return o, nil
}

// release gives up client's tuner n. Releasing a tuner the client doesn't own
// is a no-op.
func (d *device) release(ctx context.Context, n int, client string) error {
	d.opMu.Lock()
	defer d.opMu.Unlock()

	d.mu.Lock()
	o := d.owners[n]
	d.mu.Unlock()
	if o == nil || o.client != client {
		return nil
	}
	defer d.invalidate()
	return d.releaseLocked(ctx, n, o)
}

// releaseLocked untunes and unlocks tuner n. d.opMu must be held.
func (d *device) releaseLocked(ctx context.Context, n int, o *owner) error {
	d.mu.Lock()
	delete(d.owners, n)
	d.mu.Unlock()

	err := d.dev.Untune(ctx, n, o.key)
	if uerr := d.dev.Unlock(ctx, n, o.key); err == nil {
		err = uerr
	}
	if errors.Is(err, hdhr.ErrLocked) {
		return nil // someone else already has it; nothing of ours to release
	}
	return err
}

// releaseAll gives up every tuner we hold, on shutdown.
func (d *device) releaseAll(ctx context.Context) {
	d.opMu.Lock()
	defer d.opMu.Unlock()
	d.mu.Lock()
	owners := make(map[int]*owner, len(d.owners))
	for n, o := range d.owners {
		owners[n] = o
	}
	d.mu.Unlock()
	for n, o := range owners {
		if err := d.releaseLocked(ctx, n, o); err != nil {
			slog.Warn("couldn't release tuner", "device", d.id, "tuner", n, "err", err)
		}
	}
}

func newLockKey() uint32 {
	var b [4]byte
	for {
		rand.Read(b[:])
		if k := binary.BigEndian.Uint32(b[:]); k != 0 {
			return k
		}
	}
}
