package qrstream

import (
	"image"

	"github.com/srlehn/serve/internal/barcodestream"
)

// Progress reports collection state after a frame was added.
type Progress struct {
	FileID uint32
	Have   int
	Total  int
	Done   bool
}

// Collector reassembles a file from QR frames supplied in any order,
// with duplicates (as a repeating video loop produces) being free.
// Symbol extraction runs outside the core collector's lock, so callers
// can parallelize image decoding across goroutines.
type Collector struct {
	core *barcodestream.Collector
}

// NewCollector returns a collector with bounded frame storage and decoded
// output. Call Forget after consuming a stream to release its share of the
// collector's limits.
func NewCollector() *Collector {
	return &Collector{core: barcodestream.NewCollector()}
}

// Add decodes one image and registers the frame it contains.
func (c *Collector) Add(img image.Image) (Progress, error) {
	raw, err := decodeImage(img)
	if err != nil {
		return Progress{}, err
	}
	progress, err := c.core.AddBytes(raw)
	return Progress(progress), err
}

// DecodeImage extracts the raw symbol content of the QR code in img,
// running the full binarization and detection ladder without collecting.
// It exists for split deployments where extraction and collection run on
// different machines, such as a browser wasm build posting camera frames
// to a remote Collector.
func DecodeImage(img image.Image) ([]byte, error) {
	return decodeImage(img)
}

// AddBytes registers a frame from its raw symbol content.
func (c *Collector) AddBytes(raw []byte) (Progress, error) {
	progress, err := c.core.AddBytes(raw)
	return Progress(progress), err
}

// Missing lists the frame indices still needed for the given stream.
func (c *Collector) Missing(fileID uint32) []int {
	return c.core.Missing(fileID)
}

// Forget drops a stream's collected frames, freeing their memory.
func (c *Collector) Forget(fileID uint32) {
	c.core.Forget(fileID)
}

// Done reports whether any stream is complete.
func (c *Collector) Done() bool {
	return c.core.Done()
}

// File returns the first completely collected file.
func (c *Collector) File() (name string, data []byte, err error) {
	return c.core.File()
}

// FileByID returns a specific completed stream by its fileID.
func (c *Collector) FileByID(fileID uint32) (name string, data []byte, err error) {
	return c.core.FileByID(fileID)
}
