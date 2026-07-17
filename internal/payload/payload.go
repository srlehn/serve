// Package payload models repeatable file-transfer inputs independently from
// the HTTP, QR, and barcode transports that consume them.
package payload

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"

	"github.com/srlehn/serve/filebrowser"
)

// ErrTooLarge reports that a transport limit was reached while producing a
// payload.
var ErrTooLarge = errors.New(`payload exceeds transport limit`)

// Warning describes one recoverable loss or degradation while producing a
// payload.
type Warning struct {
	Path    string
	Message string
}

// Report describes recoverable production problems. Producers bound the
// retained warning list and count additional warnings in OmittedWarnings.
type Report struct {
	Warnings        []Warning
	OmittedWarnings int
}

// WarningCount returns the total number of retained and omitted warnings.
func (r Report) WarningCount() int {
	return len(r.Warnings) + r.OmittedWarnings
}

// Source is a repeatable transfer input. Size returns ok=false when producing
// the payload is streaming and its final size is not known in advance.
type Source interface {
	Filename() string
	Size() (size int64, ok bool)
	WriteTo(context.Context, io.Writer) (Report, error)
}

// File is a regular file exposed as a payload source.
type File struct {
	files fs.FS
	path  string
	name  string
	size  int64
}

// OpenFile validates name and returns a repeatable regular-file source.
func OpenFile(files fs.FS, name string) (*File, error) {
	file, info, err := filebrowser.Open(files, name)
	if err != nil {
		return nil, err
	}
	file.Close()
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("payload: %s is not a regular file", name)
	}
	return &File{
		files: files,
		path:  name,
		name:  path.Base(name),
		size:  info.Size(),
	}, nil
}

func (f *File) Filename() string {
	return f.name
}

func (f *File) Size() (int64, bool) {
	return f.size, true
}

func (f *File) WriteTo(ctx context.Context, dst io.Writer) (Report, error) {
	file, info, err := filebrowser.Open(f.files, f.path)
	if err != nil {
		return Report{}, err
	}
	defer file.Close()
	if !info.Mode().IsRegular() {
		return Report{}, fmt.Errorf("payload: %s is no longer a regular file", f.path)
	}
	_, err = io.Copy(dst, contextReader{ctx: ctx, reader: file})
	return Report{}, err
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

// ReadAll produces source in memory and rejects output larger than limit. A
// non-positive limit disables the size bound.
func ReadAll(ctx context.Context, source Source, limit int64) ([]byte, Report, error) {
	if source == nil {
		return nil, Report{}, errors.New(`payload: nil source`)
	}
	if limit > 0 {
		if size, ok := source.Size(); ok && size > limit {
			return nil, Report{}, ErrTooLarge
		}
	}

	var buffer bytes.Buffer
	var dst io.Writer = &buffer
	if limit > 0 {
		dst = &limitWriter{writer: &buffer, remaining: limit}
	}
	report, err := source.WriteTo(ctx, dst)
	if err != nil {
		return nil, report, err
	}
	return buffer.Bytes(), report, nil
}

type limitWriter struct {
	writer    io.Writer
	remaining int64
}

func (w *limitWriter) Write(p []byte) (int, error) {
	if int64(len(p)) <= w.remaining {
		n, err := w.writer.Write(p)
		w.remaining -= int64(n)
		return n, err
	}
	allowed := int(w.remaining)
	if allowed > 0 {
		n, err := w.writer.Write(p[:allowed])
		w.remaining -= int64(n)
		if err != nil {
			return n, err
		}
		if n != allowed {
			return n, io.ErrShortWrite
		}
	}
	return allowed, ErrTooLarge
}
