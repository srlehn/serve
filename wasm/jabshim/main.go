//go:build js && wasm

// The browser-side jabstream receiver, the JAB Code peer of the
// qrstream shim. Exposes
//
//	jabstreamScanFrame(imageData) -> {fileID, have, total, done, sameAsLast, name?, data?} | {error, recoverable} | {miss, reason}
//
// on the JS global with the same result protocol as the QR shim.
// Camera frames flow through one persistent jabcode Stream decoder
// (frames of a live camera are one coherent sequence), the per-frame
// CRC-32C envelope is verified before any header field is read, and
// collection state stays bounded: only the stream currently being
// assembled is held, only the most recently completed stream is
// remembered for sameAsLast suppression.
//
// A frame that holds no decodable JAB symbol or fails the envelope
// check is an unusable frame, not an error; the miss result names
// the reason so the worker can ship it to the diagnostic log while
// the page keeps treating it as a plain miss. Collector failures are
// recoverable: assembled state is dropped and the symbol decoder
// reset so the next frame starts clean.
package main

import (
	"fmt"
	"image"
	"syscall/js"

	"github.com/srlehn/jabcode"
	"github.com/srlehn/serve/jabstream"
)

// maxFramePixels bounds the RGBA allocation for one camera frame.
// The page downscales to 1024 pixels of width; this leaves headroom
// without letting a malformed message allocate gigabytes.
const maxFramePixels = 4096 * 4096

func main() {
	decoder := jabcode.NewStream()
	c := jabstream.NewCollector()
	var curID uint32 // stream currently being assembled
	var collecting bool
	var lastID uint32 // most recently completed stream
	var haveLast bool

	// The frame buffer is reused while the dimensions stay stable:
	// camera frames arrive at one fixed size, and a fresh
	// multi-megabyte allocation per frame grows the wasm heap faster
	// than the collector returns garbage. The Stream decoder copies
	// everything it carries across calls (working bitmaps, remembered
	// geometry, observation snapshots) and holds no reference into
	// the input pixels once DecodeMessage returns, so reuse is safe.
	var img *image.RGBA

	reset := func() {
		if collecting {
			c.Forget(curID)
			collecting = false
		}
		decoder.Reset()
	}

	// A miss result carries the reason so the worker can ship it to
	// the diagnostic log; the page still treats it as an unusable
	// frame.
	miss := func(reason string) js.Value {
		result := js.Global().Get(`Object`).New()
		result.Set(`miss`, true)
		result.Set(`reason`, reason)
		return result
	}

	js.Global().Set(`jabstreamScanFrame`, js.FuncOf(func(this js.Value, args []js.Value) any {
		if len(args) < 1 {
			return miss(`no image argument`)
		}
		imageData := args[0]
		w := imageData.Get(`width`).Int()
		h := imageData.Get(`height`).Int()
		if w <= 0 || h <= 0 || w > maxFramePixels/h {
			return miss(fmt.Sprintf(`bad frame dimensions %dx%d`, w, h))
		}
		if img == nil || img.Rect.Dx() != w || img.Rect.Dy() != h {
			img = image.NewRGBA(image.Rect(0, 0, w, h))
		}
		if n := js.CopyBytesToGo(img.Pix, imageData.Get(`data`)); n != len(img.Pix) {
			return miss(fmt.Sprintf(`pixel copy %d of %d bytes`, n, len(img.Pix)))
		}
		msg, err := decoder.DecodeMessage(img)
		if err != nil {
			return miss(fmt.Sprintf(`decode %dx%d: %v`, w, h, err))
		}
		if _, err := jabstream.OpenFrame(msg.Data); err != nil {
			// decoded symbol, but not an intact sealed qS frame:
			// corrupt envelope or foreign JAB content
			return miss(fmt.Sprintf(`envelope (%d symbol bytes): %v`, len(msg.Data), err))
		}
		id, ok := jabstream.FrameID(msg.Data)
		if !ok {
			return miss(`sealed frame with invalid qS header`)
		}

		result := js.Global().Get(`Object`).New()
		result.Set(`fileID`, id)

		// the just-completed stream still in view: ignore, no
		// re-collection. Only the last stream is remembered.
		if haveLast && id == lastID {
			result.Set(`sameAsLast`, true)
			return result
		}

		// only one stream is assembled at a time: a different id means
		// the previous, unfinished one was abandoned - drop it
		if collecting && curID != id {
			c.Forget(curID)
		}
		curID, collecting = id, true

		prog, err := c.AddBytes(msg.Data)
		if err != nil {
			reset()
			return setError(result, err)
		}
		result.Set(`have`, prog.Have)
		result.Set(`total`, prog.Total)
		result.Set(`done`, prog.Done)
		if prog.Done {
			name, data, err := c.FileByID(id)
			if err != nil {
				reset()
				return setError(result, err)
			}
			c.Forget(id) // free the assembled frames
			lastID, haveLast = id, true
			collecting = false
			setFile(result, name, data)
		}
		return result
	}))
	select {} // keep the Go runtime alive for callbacks
}

func setError(result js.Value, err error) js.Value {
	result.Set(`error`, err.Error())
	result.Set(`recoverable`, true)
	return result
}

func setFile(result js.Value, name string, data []byte) {
	buf := js.Global().Get(`Uint8Array`).New(len(data))
	js.CopyBytesToJS(buf, data)
	result.Set(`name`, name)
	result.Set(`data`, buf)
}
