// Package hdhr speaks the HDHomeRun control protocol (TCP and UDP port 65001),
// the same protocol hdhomerun_config uses.
//
// A packet is a big-endian type and payload length, the payload as a list of
// tag-length-value fields, and a CRC32 of everything before it, stored
// little-endian.
package hdhr

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
)

const Port = 65001

// Packet types.
const (
	typeDiscoverReq uint16 = 0x0002
	typeDiscoverRpy uint16 = 0x0003
	typeGetSetReq   uint16 = 0x0004
	typeGetSetRpy   uint16 = 0x0005
)

// Tags.
const (
	tagDeviceType   byte = 0x01
	tagDeviceID     byte = 0x02
	tagGetSetName   byte = 0x03
	tagGetSetValue  byte = 0x04
	tagErrorMessage byte = 0x05
	tagTunerCount   byte = 0x10
	tagLockKey      byte = 0x15
	tagBaseURL      byte = 0x2A
)

const (
	deviceTypeTuner    uint32 = 0x00000001
	deviceTypeWildcard uint32 = 0xFFFFFFFF
	deviceIDWildcard   uint32 = 0xFFFFFFFF
)

const maxPayload = 3074

type tlv struct {
	tag   byte
	value []byte
}

func encodePacket(pktType uint16, fields []tlv) []byte {
	var payload []byte
	for _, f := range fields {
		payload = append(payload, f.tag)
		n := len(f.value)
		if n <= 127 {
			payload = append(payload, byte(n))
		} else {
			payload = append(payload, byte(n&0x7F)|0x80, byte(n>>7))
		}
		payload = append(payload, f.value...)
	}

	pkt := make([]byte, 4, 4+len(payload)+4)
	binary.BigEndian.PutUint16(pkt[0:], pktType)
	binary.BigEndian.PutUint16(pkt[2:], uint16(len(payload)))
	pkt = append(pkt, payload...)
	return binary.LittleEndian.AppendUint32(pkt, crc32.ChecksumIEEE(pkt))
}

var errShortPacket = errors.New("hdhr: short packet")

// packetLength returns the full length of the packet whose header starts buf,
// or 0 if buf doesn't hold a whole header yet.
func packetLength(buf []byte) int {
	if len(buf) < 4 {
		return 0
	}
	return 4 + int(binary.BigEndian.Uint16(buf[2:])) + 4
}

func decodePacket(pkt []byte) (uint16, []tlv, error) {
	if len(pkt) < 8 || len(pkt) != packetLength(pkt) {
		return 0, nil, errShortPacket
	}
	body := pkt[:len(pkt)-4]
	if crc32.ChecksumIEEE(body) != binary.LittleEndian.Uint32(pkt[len(pkt)-4:]) {
		return 0, nil, errors.New("hdhr: bad packet CRC")
	}

	pktType := binary.BigEndian.Uint16(pkt)
	payload := body[4:]
	var fields []tlv
	for len(payload) > 0 {
		if len(payload) < 2 {
			return 0, nil, errShortPacket
		}
		tag, n, hdr := payload[0], int(payload[1]), 2
		if n&0x80 != 0 {
			if len(payload) < 3 {
				return 0, nil, errShortPacket
			}
			n = n&0x7F | int(payload[2])<<7
			hdr = 3
		}
		if len(payload) < hdr+n {
			return 0, nil, errShortPacket
		}
		fields = append(fields, tlv{tag, payload[hdr : hdr+n]})
		payload = payload[hdr+n:]
	}
	return pktType, fields, nil
}

func cString(s string) []byte { return append([]byte(s), 0) }

func fromCString(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}

func uint32Value(v uint32) []byte { return binary.BigEndian.AppendUint32(nil, v) }

// DeviceError is an error the device reported, such as
// "ERROR: resource locked by 10.10.0.6".
type DeviceError struct{ Message string }

func (e *DeviceError) Error() string { return fmt.Sprintf("hdhr: device: %s", e.Message) }
