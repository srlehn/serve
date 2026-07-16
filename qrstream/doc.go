// Package qrstream encodes a file into a sequence of QR-code frames
// meant to be displayed as a repeating video loop, and decodes such
// frames - in any order, with duplicates - back into the original
// file. Encoder and decoder are pure Go (no cgo, no external tools).
//
// # Pipeline
//
// The file is wrapped in a small container that preserves its name,
// compressed with zstd at maximum level (stored raw if
// incompressible), and split into chunks that exactly fill the 8-bit
// byte-mode capacity of the chosen QR version and error-correction
// level (ISO/IEC 18004). Each frame carries an 11-byte binary header:
//
//	off len field
//	0   2   magic "qS"
//	2   1   format version (high nibble) | flags (low nibble)
//	3   4   fileID: CRC-32C of the compressed container
//	7   2   seq, 0-based frame index
//	9   2   total frame count (doubles as the end marker)
//
// The fileID groups frames of the same stream, rejects stale frames,
// and verifies the reassembled payload; per-frame integrity comes from
// the QR symbol's own Reed-Solomon coding. Output is deterministic:
// the same input yields the same frames and fileID. A flags bit
// selects the rateless fountain mode (Options.Fountain), in which
// seq/total become block seed and source-block count and any
// sufficiently large frame subset reconstructs the file.
//
// # Encoding and serving
//
//	st, err := qrstream.Encode("file.bin", data, nil) // v25-Q default
//	for img, err := range st.Frames() {               // symbols in loop order,
//		...                                       // rendered on demand
//	}
//	http.Handle("/stream", st)   // live multipart motion stream
//
// # Decoding
//
//	c := qrstream.NewCollector()
//	for _, img := range stills { // photos, video frames, GIF frames
//		prog, err := c.Add(img)
//		if err == nil && prog.Done {
//			break
//		}
//	}
//	name, data, err := c.File()
//
// The Collector is frame-source-agnostic. Symbol extraction tries a
// ladder of binarization strategies (local hybrid, global histogram,
// Otsu re-threshold, 2x upscale, pure-barcode) and a multi-triple
// finder-pattern detector, which together decode hand-held photographs
// that defeat stock readers.
//
// FORMAT.md is the normative wire-format specification.
package qrstream
