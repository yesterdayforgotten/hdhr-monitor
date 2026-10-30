package server

import (
	"errors"
	"fmt"
	"net"
	"strconv"
)

// sink receives the streams the server asks ATSC 3.0 tuners to send, and
// throws them away. A 3.0 tuner only reports a station's bitrate while it is
// streaming the station somewhere, so measuring it means giving the stream a
// place to go.
type sink struct {
	conn *net.UDPConn
	port int
}

// listenSink opens the sink on UDP port; 0 picks a free port.
func listenSink(port int) (*sink, error) {
	conn, err := net.ListenUDP("udp", &net.UDPAddr{Port: port})
	if err != nil {
		return nil, fmt.Errorf("stream sink: %w", err)
	}
	s := &sink{conn: conn, port: conn.LocalAddr().(*net.UDPAddr).Port}
	go s.drain()
	return s, nil
}

func (s *sink) drain() {
	buf := make([]byte, 64<<10)
	for {
		if _, _, err := s.conn.ReadFrom(buf); errors.Is(err, net.ErrClosed) {
			return
		}
	}
}

func (s *sink) close() { s.conn.Close() }

// target is the stream target that sends a tuner of the device at host here.
// With no configured address, it is whichever of our addresses the device's
// network is reached from.
func (s *sink) target(host, configured string) (string, error) {
	addr := configured
	if addr == "" {
		h := host
		if hh, _, err := net.SplitHostPort(host); err == nil {
			h = hh
		}
		// A UDP "connection" sends nothing; it only picks the route.
		c, err := net.Dial("udp", net.JoinHostPort(h, "65001"))
		if err != nil {
			return "", fmt.Errorf("stream sink: no route to %s: %w", h, err)
		}
		addr = c.LocalAddr().(*net.UDPAddr).IP.String()
		c.Close()
	}
	return "udp://" + net.JoinHostPort(addr, strconv.Itoa(s.port)), nil
}
