package qrstream

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
)

// Frame layout: [header headerLen bytes][payload, raw bytes filling the
// remaining byte-mode capacity of the symbol].
//
//	off len field
//	0   2   magic "qS"
//	2   1   format version (high nibble) | flags (low nibble)
//	3   4   fileID: CRC-32C of the complete (compressed) container
//	7   2   seq, 0-based frame index (big endian)
//	9   2   total frame count; doubles as the end marker
const (
	magic0, magic1 = 'q', 'S'
	formatVersion  = 1
	headerLen      = 11

	flagFountain = 1 << 0 // LT fountain mode: seq/total become seed/k
	flagStore    = 1 << 1 // container stored uncompressed
)

var crcTable = crc32.MakeTable(crc32.Castagnoli)

var (
	ErrNotQRStream = errors.New("qrstream: not a qrstream frame")
	ErrVersion     = errors.New("qrstream: unsupported format version")
)

type header struct {
	flags  byte
	fileID uint32
	seq    uint16
	total  uint16
}

func (h header) marshal(dst []byte) {
	dst[0], dst[1] = magic0, magic1
	dst[2] = formatVersion<<4 | h.flags&0x0f
	binary.BigEndian.PutUint32(dst[3:7], h.fileID)
	binary.BigEndian.PutUint16(dst[7:9], h.seq)
	binary.BigEndian.PutUint16(dst[9:11], h.total)
}

// FrameID returns the stream fileID in a raw frame's header without
// collecting it, so a receiver can skip a frame whose stream it has
// already finished. ok is false if raw is not a qrstream frame.
func FrameID(raw []byte) (fileID uint32, ok bool) {
	h, _, err := parseFrame(raw)
	if err != nil {
		return 0, false
	}
	return h.fileID, true
}

func parseFrame(raw []byte) (header, []byte, error) {
	var h header
	if len(raw) < headerLen || raw[0] != magic0 || raw[1] != magic1 {
		return h, nil, ErrNotQRStream
	}
	if raw[2]>>4 != formatVersion {
		return h, nil, ErrVersion
	}
	h.flags = raw[2] & 0x0f
	h.fileID = binary.BigEndian.Uint32(raw[3:7])
	h.seq = binary.BigEndian.Uint16(raw[7:9])
	h.total = binary.BigEndian.Uint16(raw[9:11])
	// fountain seeds routinely exceed the source-block count
	if h.total == 0 || (h.flags&flagFountain == 0 && h.seq >= h.total) {
		return h, nil, fmt.Errorf("qrstream: bad frame index %d/%d", h.seq, h.total)
	}
	return h, raw[headerLen:], nil
}
