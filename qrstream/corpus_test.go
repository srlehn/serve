package qrstream

import (
	"bytes"
	"math/rand"
	"testing"
)

// TestFrameRoundTripPayloads exercises the full rendered-image pipeline with
// both compressed and stored containers at the default v25-Q symbol.
func TestFrameRoundTripPayloads(t *testing.T) {
	if testing.Short() {
		t.Skip("image decode is slow")
	}
	compressible := make([]byte, 4096)
	for i := range compressible {
		compressible[i] = byte(i)
	}
	incompressible := make([]byte, 3000)
	rand.New(rand.NewSource(3)).Read(incompressible)

	tests := []struct {
		name string
		data []byte
	}{
		{name: "compressible.bin", data: compressible},
		{name: "incompressible.bin", data: incompressible},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st, err := Encode(tt.name, tt.data, nil)
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
					t.Errorf("frame %d undecodable: %v", i, err)
				}
				i++
			}
			gotName, got, err := c.File()
			if err != nil {
				t.Fatal(err)
			}
			if gotName != tt.name || !bytes.Equal(got, tt.data) {
				t.Errorf("round trip mismatch (name=%q, %d bytes)", gotName, len(got))
			}
		})
	}
}
