package hdhr

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/yesterdayforgotten/hdhr-monitor/internal/hdhrtest"
)

func TestPacketRoundTrip(t *testing.T) {
	long := strings.Repeat("x", 300) // needs the two-byte length form
	fields := []tlv{
		{tagGetSetName, cString("/tuner0/debug")},
		{tagGetSetValue, cString(long)},
		{tagLockKey, uint32Value(0xDEADBEEF)},
	}
	pkt := encodePacket(typeGetSetReq, fields)
	if got := packetLength(pkt); got != len(pkt) {
		t.Fatalf("packetLength = %d, want %d", got, len(pkt))
	}

	typ, got, err := decodePacket(pkt)
	if err != nil {
		t.Fatal(err)
	}
	if typ != typeGetSetReq || len(got) != len(fields) {
		t.Fatalf("decoded type %#x with %d fields", typ, len(got))
	}
	for i := range fields {
		if got[i].tag != fields[i].tag || !bytes.Equal(got[i].value, fields[i].value) {
			t.Errorf("field %d = %v, want %v", i, got[i], fields[i])
		}
	}
	if fromCString(got[1].value) != long {
		t.Error("long value didn't survive")
	}
}

func TestDecodeRejectsBadPackets(t *testing.T) {
	pkt := encodePacket(typeGetSetRpy, []tlv{{tagGetSetValue, cString("ok")}})

	bad := append([]byte{}, pkt...)
	bad[5] ^= 0xFF
	if _, _, err := decodePacket(bad); err == nil {
		t.Error("corrupted packet decoded without error")
	}
	if _, _, err := decodePacket(pkt[:len(pkt)-1]); err == nil {
		t.Error("truncated packet decoded without error")
	}
}

func TestParseDebug(t *testing.T) {
	st := ParseDebug("tun: ch=8vsb:557000000 lock=8vsb:557000000 ss=77 snq=48 seq=85 dbg=8-78/107\n" +
		"dev: bps=19251200 resync=0 overflow=0\n" +
		"ts:  bps=10555072 te=9958 crc=57\n" +
		"net: bps=10547040 pps=903 err=0 stop=0\n")
	want := TunerStatus{
		Channel: "8vsb:557000000", Frequency: 557_000_000, Lock: "8vsb", Locked: true,
		Strength: 77, SNQ: 48, SymbolQuality: 85,
		DeviceBPS: 19251200, StreamBPS: 10555072, NetBPS: 10547040, NetPPS: 903,
		TransportErrors: 9958, CRCErrors: 57,
	}
	if st != want {
		t.Errorf("ParseDebug =\n%+v\nwant\n%+v", st, want)
	}
}

func TestParseDebugUnlocked(t *testing.T) {
	for _, tc := range []struct {
		in       string
		freq     int64
		lock     string
		isLocked bool
	}{
		{"tun: ch=none lock=none ss=0 snq=0 seq=0", 0, "none", false},
		{"tun: ch=auto:557000000 lock=(8vsb) ss=60 snq=0 seq=0", 557_000_000, "(8vsb)", false},
		{"", 0, "none", false},
	} {
		st := ParseDebug(tc.in)
		if st.Frequency != tc.freq || st.Lock != tc.lock || st.Locked != tc.isLocked {
			t.Errorf("ParseDebug(%q) = freq %d lock %q locked %v", tc.in, st.Frequency, st.Lock, st.Locked)
		}
	}
}

func TestParseStreamInfo(t *testing.T) {
	got := ParseStreamInfo("3: 13.1 KSTU-HD\n4: 13.2 ION\n5: 13.3 (encrypted)\ntsid=0x0BBF\n")
	want := []Program{{3, "13.1", "KSTU-HD"}, {4, "13.2", "ION"}}
	if len(got) != len(want) {
		t.Fatalf("ParseStreamInfo = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("program %d = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestRFChannel(t *testing.T) {
	for f, want := range map[int64]int{
		57_000_000:  2,
		79_000_000:  5,
		177_000_000: 7,
		213_000_000: 13,
		473_000_000: 14,
		557_000_000: 28,
		605_000_000: 36,
		557_500_000: 0,
		100_000_000: 0,
	} {
		if got := RFChannel(f); got != want {
			t.Errorf("RFChannel(%d) = %d, want %d", f, got, want)
		}
	}
}

func startFake(t *testing.T) (*hdhrtest.Device, *Device) {
	t.Helper()
	fake, err := hdhrtest.Start(2)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fake.Close)
	dev := NewDevice(fake.Addr(), 2*time.Second)
	dev.SetHTTPBase(fake.HTTPBase())
	t.Cleanup(func() { dev.Close() })
	return fake, dev
}

func TestDeviceInfoAndLineup(t *testing.T) {
	_, dev := startFake(t)
	ctx := context.Background()

	info, err := dev.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.TunerCount != 2 || info.ModelNumber != "HDFX-2US" {
		t.Errorf("Info = %+v", info)
	}
	lineup, err := dev.Lineup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(lineup) != 4 || lineup[1].GuideNumber != "13.1" {
		t.Errorf("Lineup = %+v", lineup)
	}
}

func TestTuneAndRead(t *testing.T) {
	fake, dev := startFake(t)
	ctx := context.Background()
	fake.Update(1, func(t *hdhrtest.Tuner) { t.Strength, t.SNQ, t.SymbolQuality = 78, 49, 100 })

	if err := dev.TuneVirtual(ctx, 1, "13.1", 0); err != nil {
		t.Fatal(err)
	}
	tu, err := dev.Tuner(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if tu.VChannel != "13.1" || tu.Frequency != 557_000_000 || !tu.Locked || tu.SNQ != 49 {
		t.Errorf("Tuner = %+v", tu)
	}
	if tu.InUse() {
		t.Error("an unlocked tuner with no target reported in use")
	}

	progs, err := dev.StreamInfo(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(progs) != 2 || progs[0].VChannel != "13.1" {
		t.Errorf("StreamInfo = %+v", progs)
	}

	if err := dev.TuneChannel(ctx, 0, "auto:28", 0); err != nil {
		t.Fatal(err)
	}
	if got := fake.Tuner(0).Channel; got != "auto:557000000" {
		t.Errorf("tuner 0 channel = %q", got)
	}
}

func TestLocking(t *testing.T) {
	fake, dev := startFake(t)
	ctx := context.Background()

	if err := dev.Lock(ctx, 0, 1234); err != nil {
		t.Fatal(err)
	}
	if holder, err := dev.LockHolder(ctx, 0); err != nil || holder != "127.0.0.1" {
		t.Errorf("LockHolder = %q, %v", holder, err)
	}

	// Without the key, the tuner refuses changes.
	if err := dev.TuneVirtual(ctx, 0, "2.1", 0); !errors.Is(err, ErrLocked) {
		t.Errorf("tune without key: err = %v, want ErrLocked", err)
	}
	if err := dev.Lock(ctx, 0, 999); !errors.Is(err, ErrLocked) {
		t.Errorf("second lock: err = %v, want ErrLocked", err)
	}
	// A device error keeps the connection usable.
	if err := dev.TuneVirtual(ctx, 0, "2.1", 1234); err != nil {
		t.Errorf("tune with key: %v", err)
	}

	if err := dev.Unlock(ctx, 0, 1234); err != nil {
		t.Fatal(err)
	}
	if fake.Tuner(0).LockKey != 0 {
		t.Error("still locked after Unlock")
	}

	dev.Lock(ctx, 1, 5)
	if err := dev.ForceUnlock(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if fake.Tuner(1).LockKey != 0 {
		t.Error("still locked after ForceUnlock")
	}
}

func TestClientReconnects(t *testing.T) {
	fake, err := hdhrtest.Start(1)
	if err != nil {
		t.Fatal(err)
	}
	defer fake.Close()
	c := NewClient(fake.Addr(), time.Second)
	defer c.Close()
	ctx := context.Background()

	if _, err := c.Get(ctx, "/tuner0/vchannel"); err != nil {
		t.Fatal(err)
	}
	// Drop the connection under the client; the next request redials after
	// the failed one.
	c.mu.Lock()
	c.conn.Close()
	c.mu.Unlock()
	if _, err := c.Get(ctx, "/tuner0/vchannel"); err == nil {
		t.Fatal("request on a closed connection succeeded")
	}
	if v, err := c.Get(ctx, "/tuner0/vchannel"); err != nil || v != "none" {
		t.Errorf("after reconnect: %q, %v", v, err)
	}
}

func TestClientUnreachable(t *testing.T) {
	// Start and stop a fake to get an address nothing listens on.
	fake, err := hdhrtest.Start(1)
	if err != nil {
		t.Fatal(err)
	}
	fake.Close()
	c := NewClient(fake.Addr(), 200*time.Millisecond)
	defer c.Close()
	start := time.Now()
	if _, err := c.Get(context.Background(), "/tuner0/debug"); err == nil {
		t.Fatal("request to a closed device succeeded")
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("request took %v", time.Since(start))
	}
}
