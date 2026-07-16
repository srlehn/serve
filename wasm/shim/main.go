//go:build js && wasm

// The browser-side qrstream receiver. Exposes
//
//	qrstreamScanFrame(imageData) -> {fileID, have, total, done, sameAsLast, name?, data?} | null
//
// on the JS global: the page draws a camera frame to a canvas and
// hands the ImageData here. Decoding and collection run entirely in
// the browser - the same Go pipeline as the CLI - and once a stream
// is complete the original file name and bytes are returned so the
// page can save them on the scanning device.
//
// State is minimal and the comparison is only against the single most
// recently completed stream: while that same stream stays in view
// (sameAsLast) it is ignored without re-collecting; any other stream
// - even one scanned earlier - is taken fresh. The collector holds
// only the stream currently being assembled. null means the image
// held no usable qrstream frame.
package main

import (
	"image"
	"syscall/js"

	qrstream "github.com/srlehn/serve/qrstream"
)

func main() {
	c := qrstream.NewCollector()
	var curID uint32 // stream currently being assembled
	var collecting bool
	var lastID uint32 // most recently completed stream
	var haveLast bool

	js.Global().Set(`qrstreamScanFrame`, js.FuncOf(func(this js.Value, args []js.Value) any {
		if len(args) < 1 {
			return js.Null()
		}
		imageData := args[0]
		w := imageData.Get(`width`).Int()
		h := imageData.Get(`height`).Int()
		img := image.NewRGBA(image.Rect(0, 0, w, h))
		if js.CopyBytesToGo(img.Pix, imageData.Get(`data`)) != len(img.Pix) {
			return js.Null()
		}
		raw, err := qrstream.DecodeImage(img)
		if err != nil {
			return js.Null()
		}
		id, ok := qrstream.FrameID(raw)
		if !ok {
			return js.Null()
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

		prog, err := c.AddBytes(raw)
		if err != nil {
			return js.Null()
		}
		result.Set(`have`, prog.Have)
		result.Set(`total`, prog.Total)
		result.Set(`done`, prog.Done)
		if prog.Done {
			name, data, err := c.FileByID(id)
			if err != nil {
				return js.Null()
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

func setFile(result js.Value, name string, data []byte) {
	buf := js.Global().Get(`Uint8Array`).New(len(data))
	js.CopyBytesToJS(buf, data)
	result.Set(`name`, name)
	result.Set(`data`, buf)
}
