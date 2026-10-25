package hdhr

import (
	"strconv"
	"strings"
)

// TunerStatus is a parsed /tunerN/debug reading.
type TunerStatus struct {
	Channel   string // "8vsb:557000000", "auto:28" or "none"
	Frequency int64  // Hz, 0 when not tuned
	Lock      string // modulation when locked, "none" otherwise
	Locked    bool

	Strength      int // signal strength, percent
	SNQ           int // signal-to-noise quality, percent
	SymbolQuality int // percent; 100 means no uncorrected errors

	DeviceBPS int64 // raw bitrate off the tuner
	StreamBPS int64 // bitrate of the selected program
	NetBPS    int64
	NetPPS    int64

	// Running totals since the tuner was last tuned.
	TransportErrors int64
	CRCErrors       int64
	NetErrors       int64
}

// ParseDebug parses the output of a /tunerN/debug get, for example:
//
//	tun: ch=8vsb:557000000 lock=8vsb:557000000 ss=77 snq=48 seq=85 dbg=8-78/107
//	dev: bps=19251200 resync=0 overflow=0
//	ts:  bps=10555072 te=9958 crc=57
//	net: bps=10547040 pps=903 err=0 stop=0
func ParseDebug(s string) TunerStatus {
	sections := map[string]map[string]string{}
	for _, line := range strings.Split(s, "\n") {
		name, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		sections[strings.TrimSpace(name)] = parseFields(rest)
	}

	tun, dev, ts, net := sections["tun"], sections["dev"], sections["ts"], sections["net"]
	st := TunerStatus{
		Channel:         orNone(tun["ch"]),
		Strength:        atoi(tun["ss"]),
		SNQ:             atoi(tun["snq"]),
		SymbolQuality:   atoi(tun["seq"]),
		DeviceBPS:       atoi64(dev["bps"]),
		StreamBPS:       atoi64(ts["bps"]),
		TransportErrors: atoi64(ts["te"]),
		CRCErrors:       atoi64(ts["crc"]),
		NetBPS:          atoi64(net["bps"]),
		NetPPS:          atoi64(net["pps"]),
		NetErrors:       atoi64(net["err"]),
	}

	// lock is "none", "8vsb:557000000", or a modulation in parentheses while
	// the tuner sees a carrier it can't lock to.
	lock := orNone(tun["lock"])
	mod, freq, _ := strings.Cut(lock, ":")
	st.Lock = mod
	st.Locked = mod != "none" && !strings.HasPrefix(mod, "(")
	if f := atoi64(freq); f > 0 {
		st.Frequency = f
	} else if _, f, ok := strings.Cut(st.Channel, ":"); ok && atoi64(f) > 1_000_000 {
		st.Frequency = atoi64(f)
	}
	return st
}

// parseFields parses "key=value key=value" text such as /tunerN/status.
func parseFields(s string) map[string]string {
	fields := map[string]string{}
	for _, f := range strings.Fields(s) {
		if k, v, ok := strings.Cut(f, "="); ok {
			fields[k] = v
		}
	}
	return fields
}

// Program is one entry of /tunerN/streaminfo.
type Program struct {
	Number   int    `json:"program"`
	VChannel string `json:"vchannel"`
	Name     string `json:"name"`
}

// ParseStreamInfo parses /tunerN/streaminfo, for example:
//
//	3: 13.1 KSTU-HD
//	4: 13.2 ION
//	tsid=0x0BBF
//
// Programs the device flags as encrypted or with no data are skipped.
func ParseStreamInfo(s string) []Program {
	var programs []Program
	for _, line := range strings.Split(s, "\n") {
		num, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(num))
		if err != nil || strings.Contains(rest, "(") {
			continue
		}
		vch, name, _ := strings.Cut(strings.TrimSpace(rest), " ")
		programs = append(programs, Program{n, vch, strings.TrimSpace(name)})
	}
	return programs
}

// RFChannel returns the US broadcast channel number whose center frequency
// is f (Hz), or 0 if f isn't one.
func RFChannel(f int64) int {
	mhz := f / 1_000_000
	if f%1_000_000 != 0 {
		return 0
	}
	switch {
	case mhz >= 57 && mhz <= 69 && (mhz-57)%6 == 0:
		return 2 + int(mhz-57)/6
	case mhz == 79 || mhz == 85:
		return 5 + int(mhz-79)/6
	case mhz >= 177 && mhz <= 213 && (mhz-177)%6 == 0:
		return 7 + int(mhz-177)/6
	case mhz >= 473 && mhz <= 803 && (mhz-473)%6 == 0:
		return 14 + int(mhz-473)/6
	}
	return 0
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

func atoi(s string) int { n, _ := strconv.Atoi(s); return n }

func atoi64(s string) int64 { n, _ := strconv.ParseInt(s, 10, 64); return n }
