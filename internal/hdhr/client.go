package hdhr

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"
)

// Client sends get and set requests to one device over a single TCP
// connection, reconnecting after any failure. Requests are serialized: the
// protocol has no request IDs, so replies are matched by order.
type Client struct {
	addr    string
	timeout time.Duration

	mu   sync.Mutex
	conn net.Conn
}

// NewClient returns a client for the device at host (an IP or hostname, with
// an optional port). Each request fails after timeout.
func NewClient(host string, timeout time.Duration) *Client {
	if _, _, err := net.SplitHostPort(host); err != nil {
		host = net.JoinHostPort(host, strconv.Itoa(Port))
	}
	return &Client{addr: host, timeout: timeout}
}

// Get reads a variable such as "/tuner0/status".
func (c *Client) Get(ctx context.Context, name string) (string, error) {
	return c.request(ctx, []tlv{{tagGetSetName, cString(name)}})
}

// Set writes a variable. lockKey is sent when non-zero; a tuner locked with a
// key only accepts sets that carry it.
func (c *Client) Set(ctx context.Context, name, value string, lockKey uint32) (string, error) {
	fields := []tlv{{tagGetSetName, cString(name)}, {tagGetSetValue, cString(value)}}
	if lockKey != 0 {
		fields = append(fields, tlv{tagLockKey, uint32Value(lockKey)})
	}
	return c.request(ctx, fields)
}

func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closeLocked()
}

func (c *Client) closeLocked() error {
	if c.conn == nil {
		return nil
	}
	err := c.conn.Close()
	c.conn = nil
	return err
}

func (c *Client) request(ctx context.Context, fields []tlv) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	deadline := time.Now().Add(c.timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}

	if c.conn == nil {
		d := net.Dialer{Deadline: deadline}
		conn, err := d.DialContext(ctx, "tcp", c.addr)
		if err != nil {
			return "", err
		}
		c.conn = conn
	}

	value, err := c.roundTrip(deadline, fields)
	if err != nil {
		var devErr *DeviceError
		if !errors.As(err, &devErr) {
			// The stream may be out of step; start over on the next request.
			c.closeLocked()
		}
		return "", err
	}
	return value, nil
}

func (c *Client) roundTrip(deadline time.Time, fields []tlv) (string, error) {
	if err := c.conn.SetDeadline(deadline); err != nil {
		return "", err
	}
	if _, err := c.conn.Write(encodePacket(typeGetSetReq, fields)); err != nil {
		return "", err
	}

	buf := make([]byte, 0, 512)
	chunk := make([]byte, 4096)
	for {
		if n := packetLength(buf); n > 0 && len(buf) >= n {
			if n > 4+maxPayload+4 {
				return "", fmt.Errorf("hdhr: packet too large (%d bytes)", n)
			}
			buf = buf[:n]
			break
		}
		n, err := c.conn.Read(chunk)
		if err != nil {
			return "", err
		}
		buf = append(buf, chunk[:n]...)
	}

	pktType, reply, err := decodePacket(buf)
	if err != nil {
		return "", err
	}
	if pktType != typeGetSetRpy {
		return "", fmt.Errorf("hdhr: unexpected packet type %#04x", pktType)
	}
	var value string
	for _, f := range reply {
		switch f.tag {
		case tagErrorMessage:
			return "", &DeviceError{fromCString(f.value)}
		case tagGetSetValue:
			value = fromCString(f.value)
		}
	}
	return value, nil
}
