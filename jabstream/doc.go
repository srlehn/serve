// Package jabstream encodes a file into a sequence of JAB Code
// frames meant to be displayed as a repeating video loop, as a peer
// of package qrstream. Both share the same container, fountain
// coding, collector, and qS frame format; jabstream only swaps the
// symbol technology and adds a per-frame integrity envelope.
//
// Every raw qS frame is sealed as frame || CRC-32C(frame) with four
// big-endian checksum bytes, reducing the advertised per-frame
// capacity by four. QR symbols leave residual per-frame integrity to
// Reed-Solomon plus the stream fileID; JAB Code's soft-decision LDPC
// correction of a camera-damaged color symbol can instead produce a
// confidently wrong codeword, so a receiver must verify the envelope
// before reading any header field or collecting a frame. The
// checksum detects corruption; it does not authenticate the sender.
//
// The encode side depends only on the dependency-light
// github.com/srlehn/jabcode/encoder package. Decoding camera images
// belongs to the jabcode module's reader and stays out of native
// serve builds; the browser worker feeds the decoded symbol bytes to
// OpenFrame and Collector.
//
// qrstream/FORMAT.md remains the normative wire-format specification
// for everything inside the envelope.
package jabstream
