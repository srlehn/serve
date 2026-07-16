package qrstream

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"iter"
	"math"

	"github.com/klauspost/compress/zstd"
)

// Options control symbol geometry and stream pacing. The zero value
// selects the defaults noted on each field.
type Options struct {
	Version  int     // QR symbol version 1..40; default 25
	Level    Level   // EC level; default Q
	ModulePx int     // rendered pixels per module; default 8
	FPS      float64 // frames per second for the motion stream; default 2

	// Fountain selects rateless LT coding (FORMAT.md, flags bit 0):
	// frames are random XOR combinations of the source blocks, so any
	// sufficiently large subset reconstructs the file - a receiver
	// that misses a frame never waits a full loop for it.
	Fountain bool
	// Redundancy is the loop length as a multiple of the source-block
	// count in fountain mode; default 2 (the factor txqr validated on
	// real cameras).
	Redundancy float64
}

func (o Options) withDefaults() (Options, error) {
	if o.Version == 0 {
		o.Version = 25
	}
	if o.ModulePx == 0 {
		o.ModulePx = 8
	}
	if o.FPS == 0 {
		// 2 fps: the slowest ladder stage needs ~310 ms on a
		// 1600x1200 still, and a scanner that misses a frame waits a
		// full loop for it to come around
		o.FPS = 2
	}
	if o.Redundancy == 0 {
		o.Redundancy = 2
	}
	if Capacity(o.Version, o.Level) == 0 {
		return Options{}, fmt.Errorf("qrstream: invalid version %d / level %s", o.Version, o.Level)
	}
	if o.ModulePx < 1 {
		return Options{}, fmt.Errorf("qrstream: module size must be positive, got %d", o.ModulePx)
	}
	if o.FPS <= 0 || math.IsNaN(o.FPS) || math.IsInf(o.FPS, 0) || o.FPS > 1000 {
		return Options{}, fmt.Errorf("qrstream: frame rate must be finite and in (0,1000], got %g", o.FPS)
	}
	if o.Redundancy < 1 || math.IsNaN(o.Redundancy) || math.IsInf(o.Redundancy, 0) {
		return Options{}, fmt.Errorf("qrstream: redundancy must be finite and at least 1, got %g", o.Redundancy)
	}
	return o, nil
}

// Stream is an encoded file: a fixed sequence of QR frames meant to be
// displayed in a repeating loop.
type Stream struct {
	payload []byte // (compressed) container, chunked across frames
	fileID  uint32
	flags   byte
	per     int // payload bytes per frame
	total   int // frames in one loop iteration
	opt     Options

	// fountain mode
	k       int    // source-block count
	message []byte // uvarint(len(payload)) ‖ payload, padded to k*per
}

// Encode prepares name+data as a QR frame sequence. The filename is
// stored inside the compressed container, so it costs no per-frame
// overhead and is restored verbatim by the decoder.
func Encode(name string, data []byte, opt *Options) (*Stream, error) {
	var o Options
	if opt != nil {
		o = *opt
	}
	var err error
	o, err = o.withDefaults()
	if err != nil {
		return nil, err
	}

	capacity := Capacity(o.Version, o.Level)
	per := capacity - headerLen
	if per < 1 {
		return nil, fmt.Errorf("qrstream: version %d-%v too small for header", o.Version, o.Level)
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

	if o.Fountain {
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
		n := min(int(math.Ceil(float64(k)*o.Redundancy)), math.MaxUint16+1)
		return &Stream{
			fileID:  crc32.Checksum(payload, crcTable),
			flags:   flags | flagFountain,
			per:     per,
			total:   n,
			opt:     o,
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
		opt:     o,
	}, nil
}

// NumFrames returns the number of QR symbols in one loop iteration.
func (s *Stream) NumFrames() int { return s.total }

// FileID identifies this stream; the decoder uses it to group frames
// and to verify the reassembled payload.
func (s *Stream) FileID() uint32 { return s.fileID }

// FrameBytes yields the raw symbol content of each frame in loop
// order (header + chunk). Every yielded slice is freshly allocated,
// so callers may retain them. In fountain mode each frame is the LT
// code block for seed 0, 1, 2, … (seq carries the seed, total the
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
