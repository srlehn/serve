package jabstream

import (
	"bytes"
	"math/rand/v2"
	"testing"

	"github.com/srlehn/jabcode"
)

// decodeLoop drives the full receiver pipeline the browser worker
// runs: rendered frames through one jabcode Stream decoder, sealed
// bytes through OpenFrame verification into the Collector. A small
// version keeps each frame cheap; incompressible data forces several
// frames.
func decodeLoop(t *testing.T, colors int) {
	t.Helper()
	data := make([]byte, 600)
	rand.NewChaCha8([32]byte{byte(colors)}).Read(data)
	st, err := Encode(`stream.bin`, data, &Options{
		Plan:     Plan{Colors: colors, Version: 4},
		ModulePx: 4,
	})
	if err != nil {
		t.Fatalf("%d colors: %v", colors, err)
	}
	if st.NumFrames() < 2 {
		t.Fatalf("%d colors: want a multi-frame stream, got %d", colors, st.NumFrames())
	}
	decoder := jabcode.NewStream()
	c := NewCollector()
	frames := 0
	for img, err := range st.Frames() {
		if err != nil {
			t.Fatalf("%d colors render frame %d: %v", colors, frames, err)
		}
		msg, err := decoder.DecodeMessage(img)
		if err != nil {
			t.Fatalf("%d colors decode frame %d: %v", colors, frames, err)
		}
		if _, err := c.AddBytes(msg.Data); err != nil {
			t.Fatalf("%d colors collect frame %d: %v", colors, frames, err)
		}
		frames++
	}
	name, got, err := c.FileByID(st.FileID())
	if err != nil {
		t.Fatalf("%d colors after %d frames: %v", colors, frames, err)
	}
	if name != `stream.bin` || !bytes.Equal(got, data) {
		t.Fatalf("%d colors: round-trip mismatch", colors)
	}
}

func TestJabcodeStreamDecode(t *testing.T) {
	decodeLoop(t, 8)
}
