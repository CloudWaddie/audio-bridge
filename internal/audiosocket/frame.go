// Package audiosocket implements the AudioSocket wire protocol used by
// Asterisk: a 3-byte header (kind, big-endian payload length) followed by the
// payload. See https://docs.asterisk.org for the protocol description.
package audiosocket

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Kind is the AudioSocket frame type.
type Kind uint8

const (
	// KindUUID carries the 16-byte call identifier. It must be the first frame.
	KindUUID Kind = 0x00
	// KindAudio carries 16-bit signed little-endian PCM.
	KindAudio Kind = 0x01
	// KindVideo is deprecated and ignored.
	KindVideo Kind = 0x02
	// KindDTMF carries a single DTMF digit.
	KindDTMF Kind = 0x03
	// KindError carries a human-readable error message.
	KindError Kind = 0x04
)

// HeaderSize is the size of the AudioSocket frame header.
const HeaderSize = 3

// ErrFrameTooLarge is returned when a peer announces a payload larger than the
// configured limit.
var ErrFrameTooLarge = errors.New("audiosocket: frame exceeds maximum payload size")

// String returns a stable name for the frame kind.
func (k Kind) String() string {
	switch k {
	case KindUUID:
		return "uuid"
	case KindAudio:
		return "audio"
	case KindVideo:
		return "video"
	case KindDTMF:
		return "dtmf"
	case KindError:
		return "error"
	default:
		return fmt.Sprintf("unknown(0x%02x)", uint8(k))
	}
}

// AppendFrame encodes a frame onto dst and returns the extended slice.
func AppendFrame(dst []byte, kind Kind, payload []byte) []byte {
	var hdr [HeaderSize]byte
	hdr[0] = byte(kind)
	binary.BigEndian.PutUint16(hdr[1:], uint16(len(payload)))
	dst = append(dst, hdr[:]...)
	return append(dst, payload...)
}

// ReadFrame reads exactly one frame. The returned payload aliases scratch when
// it has enough capacity, so callers must consume it before the next call.
func ReadFrame(r io.Reader, maxPayload int, scratch []byte) (Kind, []byte, error) {
	var hdr [HeaderSize]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	kind := Kind(hdr[0])
	n := int(binary.BigEndian.Uint16(hdr[1:]))
	if n > maxPayload {
		return kind, nil, fmt.Errorf("%w: %s frame of %d bytes, limit %d", ErrFrameTooLarge, kind, n, maxPayload)
	}
	if n > cap(scratch) {
		scratch = make([]byte, n)
	} else {
		scratch = scratch[:n]
	}
	if _, err := io.ReadFull(r, scratch); err != nil {
		return kind, nil, fmt.Errorf("read %s payload: %w", kind, err)
	}
	return kind, scratch, nil
}

// UUIDString normalises a UUID payload. Asterisk sends the raw 16 bytes; some
// clients send the canonical 36-character text form instead.
func UUIDString(payload []byte) (string, error) {
	switch len(payload) {
	case 16:
		return formatUUID(payload), nil
	case 36:
		s := strings.ToLower(string(payload))
		if !validUUIDText(s) {
			return "", fmt.Errorf("audiosocket: invalid uuid text %q", string(payload))
		}
		return s, nil
	default:
		return "", fmt.Errorf("audiosocket: invalid uuid payload length %d", len(payload))
	}
}

func formatUUID(b []byte) string {
	const hex = "0123456789abcdef"
	var out [36]byte
	j := 0
	for i, v := range b {
		if i == 4 || i == 6 || i == 8 || i == 10 {
			out[j] = '-'
			j++
		}
		out[j] = hex[v>>4]
		out[j+1] = hex[v&0x0f]
		j += 2
	}
	return string(out[:])
}

func validUUIDText(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
			continue
		}
		isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')
		if !isHex {
			return false
		}
	}
	return true
}
