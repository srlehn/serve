package jabstream

import (
	"encoding/binary"
	"errors"
	"hash/crc32"

	"github.com/srlehn/serve/internal/barcodestream"
)

const (
	// sealLen is the size of the envelope checksum appended to every
	// raw qS frame.
	sealLen = 4
	// minSealedLen is the smallest well-formed sealed frame: the
	// 11-byte qS header, at least one payload byte, and the checksum.
	minSealedLen = 16
)

var crcTable = crc32.MakeTable(crc32.Castagnoli)

var (
	ErrFrameTooShort = errors.New(`jabstream: sealed frame is shorter than 16 bytes`)
	ErrFrameChecksum = errors.New(`jabstream: frame checksum mismatch`)
)

// sealFrame appends CRC-32C(frame) as four big-endian bytes.
func sealFrame(frame []byte) []byte {
	return binary.BigEndian.AppendUint32(frame, crc32.Checksum(frame, crcTable))
}

// OpenFrame verifies a sealed frame's checksum and returns the raw qS
// frame inside. Receivers must call it before FrameID or
// Collector.AddBytes so that a corrupted symbol decode never reaches
// header parsing or collection.
func OpenFrame(sealed []byte) ([]byte, error) {
	if len(sealed) < minSealedLen {
		return nil, ErrFrameTooShort
	}
	frame := sealed[:len(sealed)-sealLen]
	if binary.BigEndian.Uint32(sealed[len(frame):]) != crc32.Checksum(frame, crcTable) {
		return nil, ErrFrameChecksum
	}
	return frame, nil
}

// FrameID returns the stream fileID in a sealed frame's header
// without collecting it, so a receiver can skip a frame whose stream
// it has already finished. ok is false if the envelope checksum or
// the frame header is invalid.
func FrameID(sealed []byte) (fileID uint32, ok bool) {
	frame, err := OpenFrame(sealed)
	if err != nil {
		return 0, false
	}
	return barcodestream.FrameID(frame)
}
