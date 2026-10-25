// Package hdhrtest is a fake HDHomeRun for tests: the TCP control protocol
// and the discover.json and lineup.json HTTP API, with tuners that lock,
// tune and report signal like a real device.
//
// Its packet code is written separately from package hdhr's, so the tests
// check that package against an independent reading of the protocol.
package hdhrtest

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
)

// Channel is one station of the fake lineup.
type Channel struct {
	VChannel  string
	Name      string
	Frequency int64 // Hz
}

// Tuner is the fake state of one tuner.
type Tuner struct {
	Channel  string // "none" or "auto:557000000"
	VChannel string // "none" or "13.1"
	Target   string // "none" or a stream URL

	LockKey    uint32 // 0 when unlocked
	LockHolder string // IP of the lock holder

	Strength, SNQ, SymbolQuality int
	TransportErrors              int64
}

// Device is a running fake. Its fields may be changed under Mu.
type Device struct {
	Mu       sync.Mutex
	Tuners   []*Tuner
	Lineup   []Channel
	Requests []string // "get /tuner0/debug", "set /tuner0/vchannel 13.1", …

	ln   net.Listener
	http *httptest.Server
}

// DefaultLineup is a small lineup with two stations on one frequency.
var DefaultLineup = []Channel{
	{"2.1", "KUTV2", 593_000_000},
	{"13.1", "KSTU-HD", 557_000_000},
	{"13.2", "ION", 557_000_000},
}

// Start runs a fake with n idle tuners and DefaultLineup.
func Start(n int) (*Device, error) {
	d := &Device{Lineup: DefaultLineup}
	for range n {
		d.Tuners = append(d.Tuners, &Tuner{Channel: "none", VChannel: "none", Target: "none"})
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	d.ln = ln
	go d.accept()
	d.http = httptest.NewServer(http.HandlerFunc(d.serveHTTP))
	return d, nil
}

// Addr is the control protocol's host:port.
func (d *Device) Addr() string { return d.ln.Addr().String() }

// HTTPBase is the HTTP API's base URL.
func (d *Device) HTTPBase() string { return d.http.URL }

func (d *Device) Close() {
	d.ln.Close()
	d.http.Close()
}

// Tuner returns a copy of tuner n's state.
func (d *Device) Tuner(n int) Tuner {
	d.Mu.Lock()
	defer d.Mu.Unlock()
	return *d.Tuners[n]
}

// Update changes tuner n's state under the lock.
func (d *Device) Update(n int, f func(*Tuner)) {
	d.Mu.Lock()
	defer d.Mu.Unlock()
	f(d.Tuners[n])
}

func (d *Device) serveHTTP(w http.ResponseWriter, r *http.Request) {
	d.Mu.Lock()
	defer d.Mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/discover.json":
		json.NewEncoder(w).Encode(map[string]any{
			"FriendlyName": "HDHomeRun FLEX DUO", "ModelNumber": "HDFX-2US",
			"FirmwareVersion": "20260326", "DeviceID": "10ABCDEF", "TunerCount": len(d.Tuners),
		})
	case "/lineup.json":
		var out []map[string]any
		for _, c := range d.Lineup {
			out = append(out, map[string]any{"GuideNumber": c.VChannel, "GuideName": c.Name, "HD": 1})
		}
		json.NewEncoder(w).Encode(out)
	default:
		http.NotFound(w, r)
	}
}

func (d *Device) accept() {
	for {
		conn, err := d.ln.Accept()
		if err != nil {
			return
		}
		go d.serveConn(conn)
	}
}

func (d *Device) serveConn(conn net.Conn) {
	defer conn.Close()
	peer, _, _ := net.SplitHostPort(conn.RemoteAddr().String())
	for {
		hdr := make([]byte, 4)
		if _, err := io.ReadFull(conn, hdr); err != nil {
			return
		}
		rest := make([]byte, int(binary.BigEndian.Uint16(hdr[2:]))+4)
		if _, err := io.ReadFull(conn, rest); err != nil {
			return
		}
		pkt := append(hdr, rest...)
		body := pkt[:len(pkt)-4]
		if crc32.ChecksumIEEE(body) != binary.LittleEndian.Uint32(pkt[len(pkt)-4:]) {
			return
		}
		if binary.BigEndian.Uint16(pkt) != 0x0004 {
			return
		}

		var name, value string
		var hasValue bool
		var key uint32
		for p := body[4:]; len(p) >= 2; {
			tag, n, h := p[0], int(p[1]), 2
			if n&0x80 != 0 {
				n, h = n&0x7F|int(p[2])<<7, 3
			}
			v := p[h : h+n]
			switch tag {
			case 0x03:
				name = strings.TrimRight(string(v), "\x00")
			case 0x04:
				value, hasValue = strings.TrimRight(string(v), "\x00"), true
			case 0x15:
				key = binary.BigEndian.Uint32(v)
			}
			p = p[h+n:]
		}

		var reply []byte
		if hasValue {
			reply = d.set(peer, name, value, key)
		} else {
			reply = d.get(name)
		}
		if _, err := conn.Write(reply); err != nil {
			return
		}
	}
}

func (d *Device) get(name string) []byte {
	d.Mu.Lock()
	defer d.Mu.Unlock()
	d.Requests = append(d.Requests, "get "+name)
	t, field, err := d.tuner(name)
	if err != nil {
		return replyPacket(name, "", err.Error())
	}
	switch field {
	case "debug":
		return replyPacket(name, d.debug(t), "")
	case "vchannel":
		return replyPacket(name, t.VChannel, "")
	case "target":
		return replyPacket(name, t.Target, "")
	case "lockkey":
		if t.LockKey == 0 {
			return replyPacket(name, "none", "")
		}
		return replyPacket(name, t.LockHolder, "")
	case "streaminfo":
		var b strings.Builder
		prog := 3
		for _, c := range d.Lineup {
			if t.Channel == fmt.Sprintf("auto:%d", c.Frequency) {
				fmt.Fprintf(&b, "%d: %s %s\n", prog, c.VChannel, c.Name)
				prog++
			}
		}
		b.WriteString("tsid=0x0BBF\n")
		return replyPacket(name, b.String(), "")
	}
	return replyPacket(name, "", "ERROR: unknown getset variable")
}

func (d *Device) set(peer, name, value string, key uint32) []byte {
	d.Mu.Lock()
	defer d.Mu.Unlock()
	d.Requests = append(d.Requests, "set "+name+" "+value)
	t, field, err := d.tuner(name)
	if err != nil {
		return replyPacket(name, "", err.Error())
	}

	if field == "lockkey" {
		switch value {
		case "force":
			t.LockKey, t.LockHolder = 0, ""
		case "none":
			if t.LockKey != 0 && key != t.LockKey {
				return replyPacket(name, "", "ERROR: invalid lockkey")
			}
			t.LockKey, t.LockHolder = 0, ""
		default:
			k, err := strconv.ParseUint(value, 10, 32)
			if err != nil || k == 0 {
				return replyPacket(name, "", "ERROR: invalid lockkey")
			}
			if t.LockKey != 0 && t.LockKey != uint32(k) {
				return replyPacket(name, "", "ERROR: resource locked by "+t.LockHolder)
			}
			t.LockKey, t.LockHolder = uint32(k), peer
		}
		return replyPacket(name, value, "")
	}

	if t.LockKey != 0 && key != t.LockKey {
		return replyPacket(name, "", "ERROR: resource locked by "+t.LockHolder)
	}
	switch field {
	case "channel":
		t.Channel, t.VChannel = d.channel(value), "none"
	case "vchannel":
		for _, c := range d.Lineup {
			if c.VChannel == value {
				t.Channel, t.VChannel = fmt.Sprintf("auto:%d", c.Frequency), value
				return replyPacket(name, value, "")
			}
		}
		return replyPacket(name, "", "ERROR: invalid virtual channel")
	case "target":
		t.Target = value
	default:
		return replyPacket(name, "", "ERROR: unknown getset variable")
	}
	return replyPacket(name, value, "")
}

// channel turns "auto:28" or "auto:557000000" into the frequency form.
func (d *Device) channel(value string) string {
	_, v, ok := strings.Cut(value, ":")
	n, err := strconv.ParseInt(v, 10, 64)
	if !ok || err != nil {
		return "none"
	}
	if n < 100 { // UHF RF channel
		n = 473_000_000 + 6_000_000*(n-14)
	}
	return fmt.Sprintf("auto:%d", n)
}

func (d *Device) debug(t *Tuner) string {
	lock := "none"
	if t.Channel != "none" {
		_, f, _ := strings.Cut(t.Channel, ":")
		lock = "8vsb:" + f
	}
	return fmt.Sprintf("tun: ch=%s lock=%s ss=%d snq=%d seq=%d dbg=0\n"+
		"dev: bps=19392712 resync=0 overflow=0\n"+
		"ts:  bps=0 te=%d crc=0\n"+
		"net: pps=0 err=0 stop=0\n",
		t.Channel, lock, t.Strength, t.SNQ, t.SymbolQuality, t.TransportErrors)
}

func (d *Device) tuner(name string) (*Tuner, string, error) {
	rest, ok := strings.CutPrefix(name, "/tuner")
	if !ok {
		return nil, "", fmt.Errorf("ERROR: unknown getset variable")
	}
	num, field, _ := strings.Cut(rest, "/")
	n, err := strconv.Atoi(num)
	if err != nil || n < 0 || n >= len(d.Tuners) {
		return nil, "", fmt.Errorf("ERROR: unknown getset variable")
	}
	return d.Tuners[n], field, nil
}

func replyPacket(name, value, errMsg string) []byte {
	var payload []byte
	add := func(tag byte, s string) {
		v := append([]byte(s), 0)
		payload = append(payload, tag)
		if len(v) <= 127 {
			payload = append(payload, byte(len(v)))
		} else {
			payload = append(payload, byte(len(v)&0x7F)|0x80, byte(len(v)>>7))
		}
		payload = append(payload, v...)
	}
	add(0x03, name)
	if errMsg != "" {
		add(0x05, errMsg)
	} else {
		add(0x04, value)
	}
	pkt := binary.BigEndian.AppendUint16(nil, 0x0005)
	pkt = binary.BigEndian.AppendUint16(pkt, uint16(len(payload)))
	pkt = append(pkt, payload...)
	return binary.LittleEndian.AppendUint32(pkt, crc32.ChecksumIEEE(pkt))
}
