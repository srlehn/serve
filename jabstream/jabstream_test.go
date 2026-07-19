package jabstream

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"math"
	"math/rand/v2"
	"strings"
	"testing"
)

// smallPlan keeps jab encoding cheap in tests.
var smallPlan = Plan{Colors: 8, Version: 8}

func TestCapacity(t *testing.T) {
	for _, tt := range []struct {
		plan Plan
		want int
	}{
		{Plan{Colors: 8, Version: 20}, 1886}, // measured default, minus 4 envelope bytes
		{Plan{Colors: 4, Version: 20}, 1254},
		{Plan{Colors: 8, Version: 8}, 466},
		{Plan{Colors: 4, Version: 1}, 39},
		{Plan{}, 0},                       // no defaults outside Encode
		{Plan{Colors: 3, Version: 20}, 0}, // unsupported color count
		{Plan{Colors: 16, Version: 20}, 0},
		{Plan{Colors: 8, Version: 33}, 0},
		{Plan{Colors: 8, Version: -1}, 0},
	} {
		if got := Capacity(tt.plan); got != tt.want {
			t.Errorf("Capacity(%+v) = %d, want %d", tt.plan, got, tt.want)
		}
	}
}

func TestSealFrameGoldenVector(t *testing.T) {
	// standard CRC-32C check value: crc of "123456789" is 0xE3069283
	sealed := sealFrame([]byte(`123456789`))
	want := append([]byte(`123456789`), 0xe3, 0x06, 0x92, 0x83)
	if !bytes.Equal(sealed, want) {
		t.Fatalf("sealed = %x, want %x", sealed, want)
	}
}

func TestOpenFrameTruncationAndCorruption(t *testing.T) {
	st, err := Encode(`f.bin`, bytes.Repeat([]byte{0xa5}, 64), &Options{Plan: smallPlan})
	if err != nil {
		t.Fatal(err)
	}
	var sealed []byte
	for raw := range st.FrameBytes() {
		sealed = raw
		break
	}
	frame, err := OpenFrame(sealed)
	if err != nil {
		t.Fatalf("valid frame rejected: %v", err)
	}
	if !bytes.Equal(sealed[:len(sealed)-4], frame) {
		t.Fatalf("opened frame is not the sealed prefix")
	}

	for _, tt := range []struct {
		name string
		raw  []byte
		want error
	}{
		{`nil`, nil, ErrFrameTooShort},
		{`empty`, []byte{}, ErrFrameTooShort},
		{`one short of minimum`, bytes.Repeat([]byte{1}, minSealedLen-1), ErrFrameTooShort},
		{`truncated tail`, sealed[:len(sealed)-1], ErrFrameChecksum},
		{`truncated to minimum`, sealed[:minSealedLen], ErrFrameChecksum},
		{`zero filled`, make([]byte, minSealedLen), ErrFrameChecksum},
	} {
		if _, err := OpenFrame(tt.raw); !errors.Is(err, tt.want) {
			t.Errorf("%s: err = %v, want %v", tt.name, err, tt.want)
		}
	}

	// flipping any single byte - header, payload, or each checksum
	// byte - must be caught by the envelope
	for _, i := range []int{0, 2, 3, 10, 11, len(sealed) / 2,
		len(sealed) - 4, len(sealed) - 3, len(sealed) - 2, len(sealed) - 1} {
		corrupt := bytes.Clone(sealed)
		corrupt[i] ^= 0x40
		if _, err := OpenFrame(corrupt); !errors.Is(err, ErrFrameChecksum) {
			t.Errorf("byte %d flip: err = %v, want %v", i, err, ErrFrameChecksum)
		}
		if _, ok := FrameID(corrupt); ok {
			t.Errorf("byte %d flip: FrameID accepted corrupt frame", i)
		}
	}
}

func TestFrameIDMatchesStream(t *testing.T) {
	st, err := Encode(`f.bin`, []byte(`payload`), &Options{Plan: smallPlan})
	if err != nil {
		t.Fatal(err)
	}
	for sealed := range st.FrameBytes() {
		id, ok := FrameID(sealed)
		if !ok || id != st.FileID() {
			t.Fatalf("FrameID = %08x/%t, want %08x/true", id, ok, st.FileID())
		}
	}
}

func TestAllByteValuesRoundTrip(t *testing.T) {
	data := make([]byte, 0, 1024)
	for range 4 {
		for b := range 256 {
			data = append(data, byte(b))
		}
	}
	for _, fountain := range []bool{false, true} {
		st, err := Encode(`all-bytes.bin`, data, &Options{Plan: smallPlan, Fountain: fountain})
		if err != nil {
			t.Fatal(err)
		}
		c := NewCollector()
		for sealed := range st.FrameBytes() {
			if _, err := c.AddBytes(sealed); err != nil {
				t.Fatal(err)
			}
		}
		if !c.Done() {
			t.Fatalf("fountain=%t: stream incomplete after full loop", fountain)
		}
		name, got, err := c.FileByID(st.FileID())
		if err != nil {
			t.Fatal(err)
		}
		if name != `all-bytes.bin` || !bytes.Equal(got, data) {
			t.Fatalf("fountain=%t: round-trip mismatch: name %q, %d bytes", fountain, name, len(got))
		}
	}
}

func TestFrameSizesRespectCapacity(t *testing.T) {
	capacity := Capacity(smallPlan)
	st, err := Encode(`f.bin`, bytes.Repeat([]byte{0x33}, 4096), &Options{Plan: smallPlan})
	if err != nil {
		t.Fatal(err)
	}
	for sealed := range st.FrameBytes() {
		if len(sealed) < minSealedLen || len(sealed) > capacity+sealLen {
			t.Fatalf("sealed frame length %d outside [%d,%d]", len(sealed), minSealedLen, capacity+sealLen)
		}
	}
}

func TestRenderedGeometryIsStable(t *testing.T) {
	data := make([]byte, 1200)
	rand.NewChaCha8([32]byte{2}).Read(data)
	st, err := Encode(`f.bin`, data,
		&Options{Plan: smallPlan, ModulePx: 2, MarginModules: 3})
	if err != nil {
		t.Fatal(err)
	}
	if st.NumFrames() < 2 {
		t.Fatalf("want at least 2 frames, got %d", st.NumFrames())
	}
	// version 8 is a 49-module square: 49*2px + 2*3 margin modules
	want := image.Pt(49*2+2*3*2, 49*2+2*3*2)
	if got := st.ImageDimensions(); got != want {
		t.Fatalf("ImageDimensions = %v, want %v", got, want)
	}
	frames := 0
	for img, err := range st.Frames() {
		if err != nil {
			t.Fatal(err)
		}
		if got := img.Bounds().Size(); got != want {
			t.Fatalf("frame %d size = %v, want %v", frames, got, want)
		}
		r, g, b, _ := img.At(0, 0).RGBA()
		if want := color.White; r != 0xffff || g != 0xffff || b != 0xffff {
			t.Fatalf("frame %d margin corner = %v, want %v", frames, img.At(0, 0), want)
		}
		frames++
	}
	if frames != st.NumFrames() {
		t.Fatalf("rendered %d frames, want %d", frames, st.NumFrames())
	}
}

func TestRenderingHonorsCancellation(t *testing.T) {
	st, err := Encode(`f.bin`, bytes.Repeat([]byte{0x77}, 4096), &Options{Plan: smallPlan})
	if err != nil {
		t.Fatal(err)
	}
	yields := 0
	st.Frames()(func(img image.Image, err error) bool {
		if err != nil {
			t.Fatal(err)
		}
		yields++
		return false
	})
	if yields != 1 {
		t.Fatalf("iterator yielded %d frames after cancellation, want 1", yields)
	}
}

func TestFrameCountOverflow(t *testing.T) {
	plan := Plan{Colors: 4, Version: 1}
	per := Capacity(plan) - 11 // qS header
	// incompressible data guarantees the stored container needs more
	// than 65535 frames
	data := make([]byte, per*(math.MaxUint16+1))
	r := rand.NewChaCha8([32]byte{1})
	r.Read(data)
	if _, err := Encode(`big.bin`, data, &Options{Plan: plan}); err == nil ||
		!strings.Contains(err.Error(), `exceed`) {
		t.Fatalf("oversized stream err = %v", err)
	}
}

func TestRejectionLeavesCollectorUntouched(t *testing.T) {
	st, err := Encode(`f.bin`, []byte(`payload`), &Options{Plan: smallPlan})
	if err != nil {
		t.Fatal(err)
	}
	var sealed []byte
	for raw := range st.FrameBytes() {
		sealed = raw
		break
	}
	corrupt := bytes.Clone(sealed)
	corrupt[5] ^= 0xff
	c := NewCollector()
	if _, err := c.AddBytes(corrupt); !errors.Is(err, ErrFrameChecksum) {
		t.Fatalf("corrupt frame err = %v, want %v", err, ErrFrameChecksum)
	}
	if c.Done() {
		t.Fatal("collector holds state from a rejected frame")
	}
	if _, err := c.AddBytes(sealed); err != nil {
		t.Fatalf("valid frame after rejection: %v", err)
	}
}

func TestOptionValidation(t *testing.T) {
	for _, tt := range []struct {
		name string
		opt  Options
	}{
		{`three colors`, Options{Plan: Plan{Colors: 3, Version: 8}}},
		{`sixteen colors`, Options{Plan: Plan{Colors: 16, Version: 8}}},
		{`version too large`, Options{Plan: Plan{Colors: 8, Version: 33}}},
		{`negative version`, Options{Plan: Plan{Colors: 8, Version: -1}}},
		{`negative module size`, Options{ModulePx: -1}},
		{`negative margin`, Options{MarginModules: -1}},
		{`zero fps`, Options{FPS: -1}},
		{`nan fps`, Options{FPS: math.NaN()}},
		{`excess fps`, Options{FPS: 1001}},
		{`fountain redundancy below one`, Options{Fountain: true, Redundancy: 0.5}},
		{`fountain nan redundancy`, Options{Fountain: true, Redundancy: math.NaN()}},
	} {
		if _, err := Encode(`f.bin`, []byte(`x`), &tt.opt); err == nil {
			t.Errorf("%s: invalid options accepted", tt.name)
		}
	}
	if _, err := Encode(`f.bin`, []byte(`x`), nil); err != nil {
		t.Errorf("nil options rejected: %v", err)
	}
}
