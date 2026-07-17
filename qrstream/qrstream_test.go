package qrstream

import (
	"bytes"
	"image"
	"math"
	"math/rand"
	"testing"
)

// collectFrames drains the FrameBytes iterator; the shuffled-order
// tests need all frames up front.
func collectFrames(st *Stream) [][]byte {
	var raws [][]byte
	for raw := range st.FrameBytes() {
		raws = append(raws, raw)
	}
	return raws
}

func roundTripBytes(t *testing.T, name string, data []byte, opt *Options) {
	t.Helper()
	st, err := Encode(name, data, opt)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	c := NewCollector()
	raws := collectFrames(st)
	// shuffled order with duplicates, as a camera catching a loop would
	order := rand.Perm(len(raws))
	order = append(order, order[:(len(order)+1)/2]...)
	var done bool
	for _, i := range order {
		prog, err := c.AddBytes(raws[i])
		if err != nil {
			t.Fatalf("AddBytes(%d): %v", i, err)
		}
		done = prog.Done
	}
	if !done || !c.Done() {
		t.Fatalf("collector not done; missing %v", c.Missing(st.FileID()))
	}
	gotName, gotData, err := c.File()
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	if gotName != name {
		t.Errorf("name = %q, want %q", gotName, name)
	}
	if !bytes.Equal(gotData, data) {
		t.Errorf("data mismatch: got %d bytes, want %d", len(gotData), len(data))
	}
}

func TestRoundTripBinary(t *testing.T) {
	data := make([]byte, 4096)
	for i := range data {
		data[i] = byte(i) // covers 0x00..0xff, compressible
	}
	roundTripBytes(t, "all-bytes.bin", data, &Options{Version: 15, Level: M})
}

func TestRoundTripIncompressible(t *testing.T) {
	data := make([]byte, 3000)
	rand.New(rand.NewSource(1)).Read(data)
	roundTripBytes(t, "noise.bin", data, nil)
}

func TestRoundTripUnicodeNameAndTinyFile(t *testing.T) {
	roundTripBytes(t, "café-文件.txt", []byte("x"), &Options{Version: 5, Level: H})
}

func TestRoundTripEmptyFile(t *testing.T) {
	roundTripBytes(t, "empty", nil, &Options{Version: 1, Level: L})
}

func TestDefaultOptionsUseQLevel(t *testing.T) {
	st, err := Encode("default.txt", []byte("content"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if st.opt.Level != Q {
		t.Fatalf("default level = %s, want Q", st.opt.Level)
	}
}

func TestRejectsInvalidOptions(t *testing.T) {
	tests := map[string]Options{
		"level":          {Level: Level(99)},
		"module size":    {ModulePx: -1},
		"negative fps":   {FPS: -1},
		"nan fps":        {FPS: math.NaN()},
		"infinite fps":   {FPS: math.Inf(1)},
		"redundancy":     {Fountain: true, Redundancy: 0.5},
		"nan redundancy": {Fountain: true, Redundancy: math.NaN()},
	}
	for name, opt := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := Encode("file", []byte("content"), &opt); err == nil {
				t.Fatal("Encode accepted invalid options")
			}
		})
	}
}

func TestRoundTripViaImages(t *testing.T) {
	if testing.Short() {
		t.Skip("image decode is slow")
	}
	data := make([]byte, 2500)
	rand.New(rand.NewSource(2)).Read(data)
	st, err := Encode("via-images.bin", data, &Options{Version: 15, Level: Q, ModulePx: 8})
	if err != nil {
		t.Fatal(err)
	}
	c := NewCollector()
	i := 0
	for img, err := range st.Frames() {
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if _, err := c.Add(img); err != nil {
			t.Fatalf("Add(frame %d): %v", i, err)
		}
		i++
	}
	name, got, err := c.File()
	if err != nil {
		t.Fatal(err)
	}
	if name != "via-images.bin" || !bytes.Equal(got, data) {
		t.Errorf("image round trip mismatch (name=%q, %d bytes)", name, len(got))
	}
}

func TestFramesFillCapacity(t *testing.T) {
	data := make([]byte, 10000)
	rand.New(rand.NewSource(3)).Read(data)
	opt := &Options{Version: 25, Level: Q}
	st, err := Encode("fill.bin", data, opt)
	if err != nil {
		t.Fatal(err)
	}
	want := Capacity(opt.Version, opt.Level)
	raws := collectFrames(st)
	for i, raw := range raws[:len(raws)-1] {
		if len(raw) != want {
			t.Errorf("frame %d: %d bytes, want full capacity %d", i, len(raw), want)
		}
	}
}

func TestDeterministic(t *testing.T) {
	a, _ := Encode("f", []byte("same input"), nil)
	b, _ := Encode("f", []byte("same input"), nil)
	if a.FileID() != b.FileID() {
		t.Error("same input produced different fileIDs")
	}
	ra := collectFrames(a)[0]
	rb := collectFrames(b)[0]
	if !bytes.Equal(ra, rb) {
		t.Error("same input produced different frames")
	}
}

func TestRejectsForeignAndCorrupt(t *testing.T) {
	c := NewCollector()
	if _, err := c.AddBytes([]byte("https://example.com/ordinary-qr-payload")); err == nil {
		t.Error("accepted non-qrstream frame")
	}
	st, _ := Encode("f", []byte("hello"), &Options{Version: 5, Level: M})
	raw := collectFrames(st)[0]
	raw[2] = 0xf0 // bogus format version
	if _, err := c.AddBytes(raw); err == nil {
		t.Error("accepted bogus format version")
	}
}

func TestFountainRoundTripWithLoss(t *testing.T) {
	data := make([]byte, 20000)
	rand.New(rand.NewSource(7)).Read(data) // incompressible
	st, err := Encode("fountain.bin", data, &Options{Version: 15, Level: M, Fountain: true})
	if err != nil {
		t.Fatal(err)
	}
	raws := collectFrames(st)
	if len(raws) != st.NumFrames() {
		t.Fatalf("loop has %d frames, want %d", len(raws), st.NumFrames())
	}
	want := Capacity(15, M)
	for i, raw := range raws {
		if len(raw) != want {
			t.Fatalf("fountain frame %d: %d bytes, want full capacity %d", i, len(raw), want)
		}
	}
	// drop 30% and shuffle the rest: any sufficient subset decodes
	// (fixed seeds keep this deterministic)
	rnd := rand.New(rand.NewSource(8))
	rnd.Shuffle(len(raws), func(i, j int) { raws[i], raws[j] = raws[j], raws[i] })
	kept := raws[:len(raws)*7/10]
	c := NewCollector()
	var done bool
	used := 0
	for _, raw := range kept {
		prog, err := c.AddBytes(raw)
		if err != nil {
			t.Fatal(err)
		}
		used++
		if prog.Done {
			done = true
			break
		}
	}
	if !done {
		t.Fatalf("not decoded from %d of %d frames", len(kept), len(raws))
	}
	t.Logf("decoded after %d frames (loop %d)", used, len(raws))
	name, got, err := c.File()
	if err != nil {
		t.Fatal(err)
	}
	if name != "fountain.bin" || !bytes.Equal(got, data) {
		t.Errorf("fountain round trip mismatch (name=%q, %d bytes)", name, len(got))
	}
}

func TestFountainTinyFile(t *testing.T) {
	// K=1: every frame is the single source block
	st, err := Encode("tiny", []byte("hi"), &Options{Version: 5, Level: M, Fountain: true})
	if err != nil {
		t.Fatal(err)
	}
	c := NewCollector()
	prog, err := c.AddBytes(collectFrames(st)[0])
	if err != nil {
		t.Fatal(err)
	}
	if !prog.Done {
		t.Fatalf("single-block stream not done after one frame: %+v", prog)
	}
	name, got, err := c.File()
	if err != nil {
		t.Fatal(err)
	}
	if name != "tiny" || string(got) != "hi" {
		t.Errorf("got %q/%q", name, got)
	}
}

func TestFountainDeterministic(t *testing.T) {
	opt := &Options{Version: 10, Level: Q, Fountain: true}
	a, _ := Encode("f", bytes.Repeat([]byte("x"), 2000), opt)
	b, _ := Encode("f", bytes.Repeat([]byte("x"), 2000), opt)
	if a.FileID() != b.FileID() {
		t.Error("same input produced different fileIDs")
	}
	ra, rb := collectFrames(a), collectFrames(b)
	for i := range ra {
		if !bytes.Equal(ra[i], rb[i]) {
			t.Fatalf("frame %d differs between identical encodes", i)
		}
	}
}

func TestFrames(t *testing.T) {
	st, err := Encode("g.txt", bytes.Repeat([]byte("loop "), 400), &Options{Version: 10, Level: Q})
	if err != nil {
		t.Fatal(err)
	}
	var n int
	var bounds image.Rectangle
	for img, err := range st.Frames() {
		if err != nil {
			t.Fatalf("frame %d: %v", n, err)
		}
		if n == 0 {
			bounds = img.Bounds()
		} else if img.Bounds() != bounds {
			t.Errorf("frame %d: bounds %v, want %v (geometry must be static)", n, img.Bounds(), bounds)
		}
		n++
	}
	if n != st.NumFrames() {
		t.Errorf("Frames yielded %d images, want %d", n, st.NumFrames())
	}
}
