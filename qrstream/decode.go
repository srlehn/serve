package qrstream

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"image"
	"sync"

	"github.com/klauspost/compress/zstd"
)

const (
	defaultMaxCollectedBytes = 128 << 20
	defaultMaxDecodedBytes   = 128 << 20
	defaultMaxStreams        = 16
	defaultMaxFrames         = 1 << 16
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
// It is frame-source-agnostic: feed it photos, video stills or
// synthetic images via Add, or already-extracted symbol contents via
// AddBytes. Safe for concurrent use: Add runs the expensive symbol
// extraction outside the lock, so calling it from one goroutine per
// core parallelizes image decoding.
type Collector struct {
	mu                sync.Mutex
	streams           map[uint32]*partial
	collectedBytes    int64
	collectedFrames   int
	maxCollectedBytes int64
	maxDecodedBytes   uint64
	maxStreams        int
	maxFrames         int
}

type partial struct {
	flags  byte
	total  uint16            // sequential: frame count; fountain: source-block count
	chunks map[uint16][]byte // sequential mode: chunk per seq

	// fountain mode
	blockLen int
	picker   *ltPicker
	matrix   *ltMatrix
	seen     map[uint16]bool // seeds already added
	bytes    int64
	frames   int
}

// NewCollector returns a collector with bounded frame storage and decoded
// output. Call Forget after consuming a stream to release its share of the
// collector's limits.
func NewCollector() *Collector {
	return newCollector(defaultMaxCollectedBytes, defaultMaxDecodedBytes)
}

func newCollector(maxCollectedBytes int64, maxDecodedBytes uint64) *Collector {
	return &Collector{
		streams:           make(map[uint32]*partial),
		maxCollectedBytes: maxCollectedBytes,
		maxDecodedBytes:   maxDecodedBytes,
		maxStreams:        defaultMaxStreams,
		maxFrames:         defaultMaxFrames,
	}
}

// Add decodes one image and registers the frame it contains.
func (c *Collector) Add(img image.Image) (Progress, error) {
	raw, err := decodeImage(img)
	if err != nil {
		return Progress{}, err
	}
	return c.AddBytes(raw)
}

// DecodeImage extracts the raw symbol content of the QR code in img,
// running the full binarization/detection ladder without collecting.
// It exists for split deployments where extraction and collection
// run on different machines - e.g. a browser wasm build posting
// camera frames to a remote Collector.
func DecodeImage(img image.Image) ([]byte, error) {
	return decodeImage(img)
}

// AddBytes registers a frame from its raw symbol content.
func (c *Collector) AddBytes(raw []byte) (Progress, error) {
	h, chunk, err := parseFrame(raw)
	if err != nil {
		return Progress{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	p := c.streams[h.fileID]
	newStream := p == nil
	if p == nil {
		if len(c.streams) >= c.maxStreams {
			return Progress{}, fmt.Errorf("qrstream: active stream limit of %d reached", c.maxStreams)
		}
		blockLen := max(len(chunk), 1)
		if int64(blockLen) > c.maxCollectedBytes/int64(h.total) {
			return Progress{}, fmt.Errorf("qrstream: stream %08x frame geometry exceeds %d-byte collection limit", h.fileID, c.maxCollectedBytes)
		}
		p = &partial{flags: h.flags, total: h.total, chunks: make(map[uint16][]byte)}
	} else if p.total != h.total || p.flags != h.flags {
		return Progress{}, fmt.Errorf("qrstream: frame %d/%d inconsistent with stream %08x", h.seq, h.total, h.fileID)
	}
	if h.flags&flagFountain != 0 {
		if p.matrix == nil {
			p.blockLen = len(chunk)
			p.picker = newLTPicker(int(h.total))
			p.matrix = newLTMatrix(int(h.total))
			p.seen = make(map[uint16]bool)
		}
		if len(chunk) != p.blockLen {
			return Progress{}, fmt.Errorf("qrstream: fountain block length %d inconsistent with stream %08x", len(chunk), h.fileID)
		}
		if !p.seen[h.seq] {
			if err := c.reserve(p, len(chunk)); err != nil {
				return Progress{}, err
			}
			p.seen[h.seq] = true
			p.matrix.addEquation(p.picker.indices(int64(h.seq)), bytes.Clone(chunk))
		}
		if newStream {
			c.streams[h.fileID] = p
		}
		return Progress{
			FileID: h.fileID,
			Have:   p.matrix.have(), // recovered source blocks
			Total:  int(p.total),
			Done:   p.matrix.determined(),
		}, nil
	}
	if _, dup := p.chunks[h.seq]; !dup {
		if err := c.reserve(p, len(chunk)); err != nil {
			return Progress{}, err
		}
		p.chunks[h.seq] = bytes.Clone(chunk)
	}
	if newStream {
		c.streams[h.fileID] = p
	}
	return Progress{
		FileID: h.fileID,
		Have:   len(p.chunks),
		Total:  int(p.total),
		Done:   len(p.chunks) == int(p.total),
	}, nil
}

func (c *Collector) reserve(p *partial, size int) error {
	if c.collectedFrames >= c.maxFrames {
		return fmt.Errorf("qrstream: collected frame limit of %d reached", c.maxFrames)
	}
	n := int64(size)
	if n > c.maxCollectedBytes-c.collectedBytes {
		return fmt.Errorf("qrstream: collected frame data exceeds %d-byte limit", c.maxCollectedBytes)
	}
	p.bytes += n
	p.frames++
	c.collectedBytes += n
	c.collectedFrames++
	return nil
}

// Missing lists the frame indices still needed for the given stream.
// For a fountain stream it returns nil: no specific frame is missing,
// any fresh code blocks help.
func (c *Collector) Missing(fileID uint32) []int {
	c.mu.Lock()
	defer c.mu.Unlock()
	p := c.streams[fileID]
	if p == nil || p.flags&flagFountain != 0 {
		return nil
	}
	var out []int
	for i := uint16(0); i < p.total; i++ {
		if _, ok := p.chunks[i]; !ok {
			out = append(out, int(i))
		}
	}
	return out
}

// Forget drops a stream's collected frames, freeing their memory. A
// long-running receiver calls it after pulling a completed file so
// finished streams do not accumulate; the stream's fileID is enough
// to recognize and skip its later frames without re-collecting.
func (c *Collector) Forget(fileID uint32) {
	c.mu.Lock()
	if p := c.streams[fileID]; p != nil {
		c.collectedBytes -= p.bytes
		c.collectedFrames -= p.frames
	}
	delete(c.streams, fileID)
	c.mu.Unlock()
}

// Done reports whether any stream is complete.
func (c *Collector) Done() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id := range c.streams {
		if c.complete(id) != nil {
			return true
		}
	}
	return false
}

func (c *Collector) complete(fileID uint32) *partial {
	p := c.streams[fileID]
	if p == nil {
		return nil
	}
	if p.flags&flagFountain != 0 {
		if p.matrix != nil && p.matrix.determined() {
			return p
		}
		return nil
	}
	if len(p.chunks) == int(p.total) {
		return p
	}
	return nil
}

// File returns the first completely collected file, verifying the
// payload against its fileID and restoring the original name.
func (c *Collector) File() (name string, data []byte, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id := range c.streams {
		if c.complete(id) != nil {
			return c.fileLocked(id)
		}
	}
	return "", nil, errors.New("qrstream: no complete stream collected yet")
}

// FileByID returns a specific completed stream by its fileID. A
// receiver that keeps collecting after the first file completes (a
// camera loop scanning several streams in turn) uses this to fetch
// exactly the stream that just finished, rather than File's
// arbitrary first-complete choice.
func (c *Collector) FileByID(fileID uint32) (name string, data []byte, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.complete(fileID) == nil {
		return "", nil, fmt.Errorf("qrstream: stream %08x not complete", fileID)
	}
	return c.fileLocked(fileID)
}

// fileLocked reconstructs the completed stream id; caller holds mu.
func (c *Collector) fileLocked(id uint32) (name string, data []byte, err error) {
	p := c.streams[id]
	var payload []byte
	if p.flags&flagFountain != 0 {
		p.matrix.reduce()
		msg := p.matrix.message(p.blockLen)
		plen, n := binary.Uvarint(msg)
		if n <= 0 || uint64(len(msg)-n) < plen {
			return "", nil, fmt.Errorf("qrstream: stream %08x: corrupt fountain message length", id)
		}
		payload = msg[n : n+int(plen)]
	} else {
		for i := uint16(0); i < p.total; i++ {
			payload = append(payload, p.chunks[i]...)
		}
	}
	if crc32.Checksum(payload, crcTable) != id {
		return "", nil, fmt.Errorf("qrstream: stream %08x checksum mismatch", id)
	}
	container := payload
	if p.flags&flagStore == 0 {
		r, err := zstd.NewReader(nil,
			zstd.WithDecoderConcurrency(1),
			zstd.WithDecoderMaxMemory(c.maxDecodedBytes))
		if err != nil {
			return "", nil, err
		}
		container, err = r.DecodeAll(payload, nil)
		r.Close()
		if err != nil {
			return "", nil, fmt.Errorf("qrstream: stream %08x: %w", id, err)
		}
	}
	nameLen, n := binary.Uvarint(container)
	if n <= 0 || uint64(len(container)-n) < nameLen {
		return "", nil, fmt.Errorf("qrstream: stream %08x: corrupt container", id)
	}
	return string(container[n : n+int(nameLen)]), container[n+int(nameLen):], nil
}
