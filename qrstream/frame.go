package qrstream

import "github.com/srlehn/serve/internal/barcodestream"

var (
	ErrNotQRStream = barcodestream.ErrNotQRStream
	ErrVersion     = barcodestream.ErrVersion
)

// FrameID returns the stream fileID in a raw frame's header without
// collecting it, so a receiver can skip a frame whose stream it has
// already finished. ok is false if raw is not a qrstream frame.
func FrameID(raw []byte) (fileID uint32, ok bool) {
	return barcodestream.FrameID(raw)
}
