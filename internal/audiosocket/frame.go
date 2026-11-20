// Package audiosocket implements the AudioSocket wire protocol used by
// Asterisk: a 3-byte header (kind, big-endian payload length) followed by the
// payload.
//
// The kind values follow the current protocol specification at
// https://docs.asterisk.org/Configuration/Channel-Drivers/AudioSocket/ and
// Asterisk's include/asterisk/res_audiosocket.h.
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
	// KindHangup terminates the call. Payload is empty.
	KindHangup Kind = 0x00
	// KindUUID carries the 16-byte binary call identifier.
	KindUUID Kind = 0x01
	// KindDTMF carries a single ASCII DTMF digit.
	KindDTMF Kind = 0x03
	// KindAudio carries 16-bit signed little-endian mono PCM at 8 kHz.
	KindAudio Kind = 0x10
	// KindAudioSlin12 carries slin PCM at 12 kHz.
	KindAudioSlin12 Kind = 0x11
	// KindAudioSlin16 carries slin PCM at 16 kHz.
	KindAudioSlin16 Kind = 0x12
	// KindAudioSlin24 carries slin PCM at 24 kHz.
	KindAudioSlin24 Kind = 0x13
	// KindAudioSlin32 carries slin PCM at 32 kHz.
	KindAudioSlin32 Kind = 0x14
	// KindAudioSlin44 carries slin PCM at 44.1 kHz.
	KindAudioSlin44 Kind = 0x15
	// KindAudioSlin48 carries slin PCM at 48 kHz.
	KindAudioSlin48 Kind = 0x16
	// KindAudioSlin96 carries slin PCM at 96 kHz.
	KindAudioSlin96 Kind = 0x17
	// KindAudioSlin192 carries slin PCM at 192 kHz.
	KindAudioSlin192 Kind = 0x18
	// KindError reports an error; the payload is an optional error code.
	KindError Kind = 0xFF
)

// HeaderSize is the size of the AudioSocket frame header.
const HeaderSize = 3

// ErrFrameTooLarge is returned when a peer announces a payload larger than the
// configured limit.
var ErrFrameTooLarge = errors.New("audiosocket: frame exceeds maximum payload size")

// String returns a stable name for the frame kind.
func (k Kind) String() string {
	switch k {
	case KindHangup:
		return "hangup"
	case KindUUID:
		return "uuid"
	case KindDTMF:
		return "dtmf"
	case KindAudio:
		return "audio/8000"
	case KindAudioSlin12:
		return "audio/12000"
	case KindAudioSlin16:
		return "audio/16000"
	case KindAudioSlin24:
		return "audio/24000"
	case KindAudioSlin32:
		return "audio/32000"
	case KindAudioSlin44:
		return "audio/44100"
	case KindAudioSlin48:
		return "audio/48000"
	case KindAudioSlin96:
		return "audio/96000"
	case KindAudioSlin192:
		return "audio/192000"
	case KindError:
		return "error"
	default:
		return fmt.Sprintf("unknown(0x%02x)", uint8(k))
	}
}

// IsAudio reports whether k is one of the signed-linear PCM frame kinds.
func (k Kind) IsAudio() bool {
	return k >= KindAudio && k <= KindAudioSlin192
}

// Rate returns the sample rate encoded by an audio frame kind.
func (k Kind) Rate() (int, bool) {
	switch k {
	case KindAudio:
		return 8000, true
	case KindAudioSlin12:
		return 12000, true
	case KindAudioSlin16:
		return 16000, true
	case KindAudioSlin24:
		return 24000, true
	case KindAudioSlin32:
		return 32000, true
	case KindAudioSlin44:
		return 44100, true
	case KindAudioSlin48:
		return 48000, true
	case KindAudioSlin96:
		return 96000, true
	case KindAudioSlin192:
		return 192000, true
	default:
		return 0, false
	}
}

// AudioKindForRate maps a sample rate to its protocol frame kind.
func AudioKindForRate(rate int) (Kind, bool) {
	switch rate {
	case 8000:
		return KindAudio, true
	case 12000:
		return KindAudioSlin12, true
	case 16000:
		return KindAudioSlin16, true
	case 24000:
		return KindAudioSlin24, true
	case 32000:
		return KindAudioSlin32, true
	case 44100:
		return KindAudioSlin44, true
	case 48000:
		return KindAudioSlin48, true
	case 96000:
		return KindAudioSlin96, true
	case 192000:
		return KindAudioSlin192, true
	default:
		return 0, false
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

// AppendHangup appends the three-byte hangup indication.
func AppendHangup(dst []byte) []byte { return AppendFrame(dst, KindHangup, nil) }

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
