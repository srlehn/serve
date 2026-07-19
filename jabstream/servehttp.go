//go:build !(js && wasm)

// Kept out of wasm builds for the same reason as qrstream's sender:
// the live multipart stream is meaningless inside a browser worker.

package jabstream

import (
	"bytes"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strconv"
	"time"
)

// ServeHTTP streams the frame loop as a multipart/x-mixed-replace
// motion stream with lossless PNG frames (sharp module edges and
// exact palette colors, which JAB decoding depends on far more than
// monochrome QR does), until the client disconnects.
func (s *Stream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	mw := multipart.NewWriter(w)
	w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary="+mw.Boundary())
	w.WriteHeader(http.StatusOK)
	interval := time.Duration(float64(time.Second) / s.opt.FPS)
	tick := time.NewTicker(interval)
	defer tick.Stop()
	var buf bytes.Buffer
	for {
		for img, err := range s.Frames() {
			if err != nil {
				return
			}
			// a part is only terminated by the *next* boundary (RFC
			// 2046), which here arrives a full interval later; the
			// per-part Content-Length lets the browser display each
			// frame completely the moment its bytes are in
			buf.Reset()
			if err := png.Encode(&buf, img); err != nil {
				return
			}
			part, err := mw.CreatePart(textproto.MIMEHeader{
				"Content-Type":   {"image/png"},
				"Content-Length": {strconv.Itoa(buf.Len())},
			})
			if err != nil {
				return
			}
			if _, err := part.Write(buf.Bytes()); err != nil {
				return
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			select {
			case <-tick.C:
			case <-r.Context().Done():
				return
			}
		}
	}
}
