package jabstream

import (
	"github.com/srlehn/serve/internal/barcodestream"
)

// Progress reports collection state after a frame was added.
type Progress struct {
	FileID uint32
	Have   int
	Total  int
	Done   bool
}

// Collector reassembles a file from sealed JAB frames supplied in any
// order, with duplicates (as a repeating video loop produces) being
// free. Decoding symbol images into sealed bytes happens upstream in
// the jabcode reader; the collector only accepts frames whose
// envelope checksum verifies.
type Collector struct {
	core *barcodestream.Collector
}

// NewCollector returns a collector with bounded frame storage and decoded
// output. Call Forget after consuming a stream to release its share of the
// collector's limits.
func NewCollector() *Collector {
	return &Collector{core: barcodestream.NewCollector()}
}

// AddBytes verifies and registers a frame from its sealed symbol
// content. A frame that fails the envelope check is rejected before
// any header field is read or collector state is touched.
func (c *Collector) AddBytes(sealed []byte) (Progress, error) {
	frame, err := OpenFrame(sealed)
	if err != nil {
		return Progress{}, err
	}
	progress, err := c.core.AddBytes(frame)
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
