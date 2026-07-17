package barcodestream

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"iter"
	"math"

	"github.com/klauspost/compress/zstd"
)

type encodeOptions struct {
	fountain   bool
	redundancy float64
}

// Option configures stream framing.
type Option func(*encodeOptions) error

// WithFountain selects rateless LT coding and sets the number of emitted
// frames as a multiple of the source-block count.
func WithFountain(redundancy float64) Option {
	return func(options *encodeOptions) error {
		if redundancy < 1 || math.IsNaN(redundancy) || math.IsInf(redundancy, 0) {
			return fmt.Errorf("qrstream: redundancy must be finite and at least 1, got %g", redundancy)
		}
		options.fountain = true
		options.redundancy = redundancy
		return nil
	}
}

// Stream is an encoded payload split into barcode-sized frames.
type Stream struct {
	payload []byte // (compressed) container, chunked across frames
	fileID  uint32
	flags   byte
	per     int // payload bytes per frame
	total   int // frames in one loop iteration

	// fountain mode
	k       int    // source-block count
	message []byte // uvarint(len(payload)) + payload, padded to k*per
}

// Encode prepares name and data for a barcode whose raw byte capacity is
// frameCapacity. The filename is stored inside the compressed container.
func Encode(name string, data []byte, frameCapacity int, options ...Option) (*Stream, error) {
	var config encodeOptions
	for _, option := range options {
		if option == nil {
			return nil, fmt.Errorf("qrstream: nil stream option")
		}
		if err := option(&config); err != nil {
			return nil, err
		}
	}

	per := frameCapacity - headerLen
	if per < 1 {
		return nil, fmt.Errorf("qrstream: frame capacity %d is too small for header", frameCapacity)
	}

	// container: uvarint(len(name)) | name | data
	container := make([]byte, 0, binary.MaxVarintLen64+len(name)+len(data))
	container = binary.AppendUvarint(container, uint64(len(name)))
	container = append(container, name...)
	container = append(container, data...)

	// concurrency 1 keeps the output deterministic; the fileID is the
	// integrity check, so the zstd frame CRC stays off
	w, err := zstd.NewWriter(nil,
		zstd.WithEncoderLevel(zstd.SpeedBestCompression),
		zstd.WithEncoderCRC(false),
		zstd.WithEncoderConcurrency(1))
	if err != nil {
		return nil, err
	}
	defer w.Close()

	payload, flags := w.EncodeAll(container, nil), byte(0)
	if len(payload) >= len(container) {
		payload, flags = container, flagStore
	}

	if config.fountain {
		// the message carries its own length because the padding to
		// k full blocks is not recoverable from k alone
		msg := binary.AppendUvarint(nil, uint64(len(payload)))
		msg = append(msg, payload...)
		k := (len(msg) + per - 1) / per
		if k > math.MaxUint16 {
			return nil, fmt.Errorf("qrstream: %d source blocks exceed format limit %d; use a larger symbol", k, math.MaxUint16)
		}
		msg = append(msg, make([]byte, k*per-len(msg))...)
		// seeds are uint16, so one loop holds at most 65536 distinct frames
		n := min(int(math.Ceil(float64(k)*config.redundancy)), math.MaxUint16+1)
		return &Stream{
			fileID:  crc32.Checksum(payload, crcTable),
			flags:   flags | flagFountain,
			per:     per,
			total:   n,
			k:       k,
			message: msg,
		}, nil
	}

	total := (len(payload) + per - 1) / per
	if total > math.MaxUint16 {
		return nil, fmt.Errorf("qrstream: %d frames exceed format limit %d; use a larger symbol", total, math.MaxUint16)
	}

	return &Stream{
		payload: payload,
		fileID:  crc32.Checksum(payload, crcTable),
		flags:   flags,
		per:     per,
		total:   total,
	}, nil
}

// NumFrames returns the number of barcode symbols in one loop iteration.
func (s *Stream) NumFrames() int { return s.total }

// FileID identifies this stream; the decoder uses it to group frames
// and to verify the reassembled payload.
func (s *Stream) FileID() uint32 { return s.fileID }

// FrameBytes yields the raw symbol content of each frame in loop
// order (header + chunk). Every yielded slice is freshly allocated,
// so callers may retain them. In fountain mode each frame is the LT
// code block for seed 0, 1, 2, ... (seq carries the seed, total the
// source-block count).
func (s *Stream) FrameBytes() iter.Seq[[]byte] {
	if s.flags&flagFountain != 0 {
		return s.fountainFrameBytes()
	}
	return func(yield func([]byte) bool) {
		for i := 0; i < s.total; i++ {
			lo := i * s.per
			hi := min(lo+s.per, len(s.payload))
			raw := make([]byte, headerLen+hi-lo)
			header{
				flags:  s.flags,
				fileID: s.fileID,
				seq:    uint16(i),
				total:  uint16(s.total),
			}.marshal(raw)
			copy(raw[headerLen:], s.payload[lo:hi])
			if !yield(raw) {
				return
			}
		}
	}
}

func (s *Stream) fountainFrameBytes() iter.Seq[[]byte] {
	return func(yield func([]byte) bool) {
		p := newLTPicker(s.k)
		for seed := 0; seed < s.total; seed++ {
			raw := make([]byte, headerLen+s.per)
			header{
				flags:  s.flags,
				fileID: s.fileID,
				seq:    uint16(seed),
				total:  uint16(s.k),
			}.marshal(raw)
			chunk := raw[headerLen:]
			for _, idx := range p.indices(int64(seed)) {
				xorBytes(chunk, s.message[idx*s.per:(idx+1)*s.per])
			}
			if !yield(raw) {
				return
			}
		}
	}
}
