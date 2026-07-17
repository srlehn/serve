# qrstream

Serve a file as a looping video stream of QR codes; decode it back from
GIFs, photos, or video stills. Pure Go - no cgo, no external tools.

- file → zstd-compressed container (filename inside) → chunks that
  exactly fill the QR symbol's byte-mode capacity → frames with an
  11-byte binary header (`fileID`, `seq`, `total`)
- decoder collects frames in any order, duplicates are free, progress
  and missing-frame reporting included, CRC-32C-verified reassembly
- robust symbol extraction: binarizer ladder + multi-triple
  finder-pattern detection (validated against a hand-held photo corpus
  that defeats zbar and stock zxing - real-camera behavior is only
  exercised by camera use, not by the shipped test suite)

## Documentation

- [FORMAT.md](FORMAT.md) defines the wire format, frame header,
  container, and flags.
- `doc.go` contains API documentation for `go doc -all .`.

## Library

```go
st, _ := qrstream.Encode("file.bin", data, nil)  // v25-Q, 8 px/module, 2 fps
for img, err := range st.Frames() {              // symbols in loop order,
    ...                                          // rendered on demand
}
http.Handle("/stream", st)                       // motion video stream (PNG frames)

c := qrstream.NewCollector()
for _, img := range stills {
    if prog, err := c.Add(img); err == nil && prog.Done {
        break
    }
}
name, data, _ := c.File()
```

## CLI

```sh
# encode a file into a looping QR GIF
go run ./qrstream/cmd/qrstream encode -o out.qr.gif path/to/file

# ... rateless: any sufficient frame subset decodes (no missed-frame
# full-loop penalty)
go run ./qrstream/cmd/qrstream encode -fountain -o out.qr.gif path/to/file

# ... or serve it as a motion video stream
go run ./qrstream/cmd/qrstream encode -serve :8080 path/to/file

# decode from a GIF (or any mix of GIFs/photos); -o dir writes the
# file under its stored name, default is stdout
go run ./qrstream/cmd/qrstream decode -o outdir out.qr.gif
go run ./qrstream/cmd/qrstream decode -o outdir photos/*.jpg

# both ends speak stdin/stdout ("-" or no argument); inputs are
# sniffed by content, so the CLI works as a filter
tar cz . | qrstream encode -name backup.tar.gz - | qrstream decode - > out.tar.gz
```

Flags for `encode`: `-ver` (QR version, default 25), `-level` (L/M/Q/H,
default Q), `-px` (pixels per module, default 8), `-fps` (default 2),
`-name` (stored file name for stdin input).

## Dependencies

All dependencies are pure Go:

- [github.com/klauspost/compress](https://github.com/klauspost/compress)
  provides zstd compression.
- [github.com/skip2/go-qrcode](https://github.com/skip2/go-qrcode)
  provides forced-version QR encoding.
- [github.com/makiuchi-d/gozxing](https://github.com/makiuchi-d/gozxing)
  provides QR decoding and detector internals.

## Layout

```text
*.go                         QR capacity, rendering, and image detection
../internal/barcodestream/   container, framing, fountain code, collection
cmd/qrstream/                CLI: encode | decode
```

## Tests

```sh
go test ./...        # full suite incl. frame round trips
go test -short ./... # skip the slow image-decoding tests
```
