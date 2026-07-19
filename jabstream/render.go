package jabstream

import (
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"iter"
)

// Frames yields the symbols of one loop iteration in order, rendered
// on demand; nothing is kept beyond the yielded frame, and breaking
// out of the loop stops rendering. Iteration stops after yielding the
// first error. All frames of a stream share one fixed plan, so
// geometry and finder positions stay static for a scanning camera.
// The white margin around the symbol is the quiet zone.
func (s *Stream) Frames() iter.Seq2[image.Image, error] {
	margin := s.opt.MarginModules * s.opt.ModulePx
	return func(yield func(image.Image, error) bool) {
		i := 0
		for sealed := range s.FrameBytes() {
			symbol, err := s.plan.Encode(sealed)
			if err != nil {
				yield(nil, fmt.Errorf(`jabstream: frame %d: %w`, i, err))
				return
			}
			if !yield(withMargin(symbol, margin), nil) {
				return
			}
			i++
		}
	}
}

// ImageDimensions returns the fixed pixel size every rendered frame
// shares, including the margin.
func (s *Stream) ImageDimensions() image.Point {
	d := s.plan.ImageDimensions()
	m := 2 * s.opt.MarginModules * s.opt.ModulePx
	return image.Pt(d.X+m, d.Y+m)
}

// FPS returns the configured display rate of the motion stream.
func (s *Stream) FPS() float64 {
	return s.opt.FPS
}

func withMargin(symbol image.Image, margin int) image.Image {
	if margin <= 0 {
		return symbol
	}
	bounds := symbol.Bounds()
	framed := image.NewRGBA(image.Rect(0, 0, bounds.Dx()+2*margin, bounds.Dy()+2*margin))
	draw.Draw(framed, framed.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	draw.Draw(framed, image.Rect(margin, margin, margin+bounds.Dx(), margin+bounds.Dy()),
		symbol, bounds.Min, draw.Src)
	return framed
}
