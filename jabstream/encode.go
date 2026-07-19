package jabstream

import (
	"fmt"
	"image"
	"iter"
	"math"

	"github.com/srlehn/jabcode/encoder"
	"github.com/srlehn/serve/internal/barcodestream"
)

// Plan is a fixed JAB Code symbol plan: one square primary symbol
// whose color count and version never change within a stream, so
// every frame shares exact geometry and byte capacity.
type Plan struct {
	Colors  int // module colors, 4 or 8; default 8
	Version int // square side version 1..32; default 20
}

// The zero value selects the measured default: 8 colors at version
// 20x20 carry 1890 raw bytes per symbol in a 97-module square, which
// still leaves over 10 camera pixels per module on a 1024-pixel
// scanner frame. 4 colors at the same geometry carry only 1258 bytes
// with no decode-time advantage on clean frames, so they stay an
// option for hostile color conditions rather than the default.
func (p Plan) withDefaults() Plan {
	if p.Colors == 0 {
		p.Colors = 8
	}
	if p.Version == 0 {
		p.Version = 20
	}
	return p
}

func (p Plan) opaquePlan(modulePx int) (*encoder.OpaquePlan, error) {
	// ISO/IEC 23634 defines higher color modes, but they are not
	// exposed here until real camera transfers justify them.
	if p.Colors != 4 && p.Colors != 8 {
		return nil, fmt.Errorf(`jabstream: color count must be 4 or 8, got %d`, p.Colors)
	}
	plan, err := encoder.NewOpaquePlan(image.Pt(p.Version, p.Version),
		encoder.WithColors(p.Colors), encoder.WithModuleSize(modulePx))
	if err != nil {
		return nil, fmt.Errorf(`jabstream: %w`, err)
	}
	return plan, nil
}

// Capacity returns the payload bytes per frame a plan can carry after
// the four envelope checksum bytes, or 0 for an invalid plan.
func Capacity(plan Plan) int {
	p, err := plan.opaquePlan(1)
	if err != nil {
		return 0
	}
	return max(p.Capacity()-sealLen, 0)
}

// Options control symbol geometry and stream pacing. The zero value
// selects the defaults noted on each field.
type Options struct {
	Plan Plan // fixed symbol plan; default 8 colors, version 20

	// ModulePx is rendered pixels per module; default 8.
	ModulePx int
	// MarginModules is the white quiet zone around the symbol, in
	// modules per side; default 2.
	MarginModules int
	// FPS is frames per second for the motion stream; default 2.
	FPS float64

	// Fountain selects rateless LT coding exactly as in qrstream:
	// frames are random XOR combinations of the source blocks, so any
	// sufficiently large subset reconstructs the file.
	Fountain bool
	// Redundancy is the loop length as a multiple of the source-block
	// count in fountain mode; default 2.
	Redundancy float64
}

func (o Options) withDefaults() (Options, error) {
	o.Plan = o.Plan.withDefaults()
	if o.ModulePx == 0 {
		o.ModulePx = 8
	}
	if o.MarginModules == 0 {
		o.MarginModules = 2
	}
	if o.FPS == 0 {
		o.FPS = 2
	}
	if o.Redundancy == 0 {
		o.Redundancy = 2
	}
	if o.ModulePx < 1 {
		return Options{}, fmt.Errorf(`jabstream: module size must be positive, got %d`, o.ModulePx)
	}
	if o.MarginModules < 0 {
		return Options{}, fmt.Errorf(`jabstream: margin must not be negative, got %d`, o.MarginModules)
	}
	if o.FPS <= 0 || math.IsNaN(o.FPS) || math.IsInf(o.FPS, 0) || o.FPS > 1000 {
		return Options{}, fmt.Errorf(`jabstream: frame rate must be finite and in (0,1000], got %g`, o.FPS)
	}
	if o.Redundancy < 1 || math.IsNaN(o.Redundancy) || math.IsInf(o.Redundancy, 0) {
		return Options{}, fmt.Errorf(`jabstream: redundancy must be finite and at least 1, got %g`, o.Redundancy)
	}
	return o, nil
}

// Stream is an encoded file: a fixed sequence of JAB Code frames
// meant to be displayed in a repeating loop.
type Stream struct {
	core *barcodestream.Stream
	plan *encoder.OpaquePlan
	opt  Options
}

// Encode prepares name and data as a JAB Code frame sequence. The
// filename is stored inside the compressed container, so it costs no
// per-frame overhead and is restored verbatim by the decoder.
func Encode(name string, data []byte, options *Options) (*Stream, error) {
	var configured Options
	if options != nil {
		configured = *options
	}
	configured, err := configured.withDefaults()
	if err != nil {
		return nil, err
	}
	plan, err := configured.Plan.opaquePlan(configured.ModulePx)
	if err != nil {
		return nil, err
	}

	var streamOptions []barcodestream.Option
	if configured.Fountain {
		streamOptions = append(streamOptions, barcodestream.WithFountain(configured.Redundancy))
	}
	core, err := barcodestream.Encode(name, data, plan.Capacity()-sealLen, streamOptions...)
	if err != nil {
		return nil, err
	}
	return &Stream{core: core, plan: plan, opt: configured}, nil
}

// NumFrames returns the number of JAB symbols in one loop iteration.
func (s *Stream) NumFrames() int {
	return s.core.NumFrames()
}

// FileID identifies this stream; the decoder uses it to group frames
// and to verify the reassembled payload.
func (s *Stream) FileID() uint32 {
	return s.core.FileID()
}

// FrameBytes yields the sealed symbol content of each frame in loop
// order: the raw qS frame followed by its envelope checksum. Every
// yielded slice is freshly allocated, so callers may retain them.
func (s *Stream) FrameBytes() iter.Seq[[]byte] {
	return func(yield func([]byte) bool) {
		for raw := range s.core.FrameBytes() {
			if !yield(sealFrame(raw)) {
				return
			}
		}
	}
}
