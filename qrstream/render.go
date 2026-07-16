package qrstream

import (
	"fmt"
	"image"
	"iter"

	"github.com/skip2/go-qrcode"
)

// Frames yields the symbols of one loop iteration in order, rendered
// on demand; nothing is kept beyond the yielded frame. Iteration
// stops after yielding the first error. All frames of a stream share
// version and therefore geometry, which keeps the finder patterns
// static for a scanning camera. A negative pixel scale in skip2
// means pixels per module, including the standard 4-module quiet
// zone.
func (s *Stream) Frames() iter.Seq2[image.Image, error] {
	return func(yield func(image.Image, error) bool) {
		i := 0
		for raw := range s.FrameBytes() {
			q, err := qrcode.NewWithForcedVersion(string(raw), s.opt.Version, s.opt.Level.recovery())
			if err != nil {
				yield(nil, fmt.Errorf("qrstream: frame %d: %w", i, err))
				return
			}
			if !yield(q.Image(-s.opt.ModulePx), nil) {
				return
			}
			i++
		}
	}
}
