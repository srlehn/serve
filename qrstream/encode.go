package qrstream

import (
	"fmt"
	"iter"
	"math"

	"github.com/srlehn/serve/internal/barcodestream"
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
		// 2 fps: the slowest ladder stage needs about 310 ms on a
		// 1600x1200 still, and a scanner that misses a frame waits a
		// full loop for it to come around.
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
	core *barcodestream.Stream
	opt  Options
}

// Encode prepares name and data as a QR frame sequence. The filename is
// stored inside the compressed container, so it costs no per-frame overhead
// and is restored verbatim by the decoder.
func Encode(name string, data []byte, options *Options) (*Stream, error) {
	var configured Options
	if options != nil {
		configured = *options
	}
	configured, err := configured.withDefaults()
	if err != nil {
		return nil, err
	}

	var streamOptions []barcodestream.Option
	if configured.Fountain {
		streamOptions = append(streamOptions, barcodestream.WithFountain(configured.Redundancy))
	}
	core, err := barcodestream.Encode(
		name,
		data,
		Capacity(configured.Version, configured.Level),
		streamOptions...,
	)
	if err != nil {
		return nil, err
	}
	return &Stream{core: core, opt: configured}, nil
}

// NumFrames returns the number of QR symbols in one loop iteration.
func (s *Stream) NumFrames() int {
	return s.core.NumFrames()
}

// FileID identifies this stream; the decoder uses it to group frames
// and to verify the reassembled payload.
func (s *Stream) FileID() uint32 {
	return s.core.FileID()
}

// FrameBytes yields the raw symbol content of each frame in loop order.
func (s *Stream) FrameBytes() iter.Seq[[]byte] {
	return s.core.FrameBytes()
}
