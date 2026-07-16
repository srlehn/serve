package main

import (
	"bytes"
	"testing"

	qrstream "github.com/srlehn/serve/qrstream"
)

func TestBuildGIF(t *testing.T) {
	st, err := qrstream.Encode("g.txt", bytes.Repeat([]byte("loop "), 400), &qrstream.Options{Version: 10, Level: qrstream.Q})
	if err != nil {
		t.Fatal(err)
	}
	g, err := buildGIF(st, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Image) != st.NumFrames() || g.LoopCount != 0 {
		t.Errorf("GIF: %d frames (want %d), loop %d (want 0)", len(g.Image), st.NumFrames(), g.LoopCount)
	}
	if g.Delay[0] != 20 {
		t.Errorf("delay = %d, want 20 (5 fps)", g.Delay[0])
	}
}
