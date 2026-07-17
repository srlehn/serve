package payload

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"testing/fstest"
)

func TestFileSource(t *testing.T) {
	source, err := OpenFile(fstest.MapFS{
		`dir/file.txt`: {Data: []byte(`content`)},
	}, `dir/file.txt`)
	if err != nil {
		t.Fatal(err)
	}
	if source.Filename() != `file.txt` {
		t.Fatalf("filename = %q", source.Filename())
	}
	if size, ok := source.Size(); !ok || size != 7 {
		t.Fatalf("size = %d, %t", size, ok)
	}
	for range 2 {
		var output bytes.Buffer
		report, err := source.WriteTo(context.Background(), &output)
		if err != nil {
			t.Fatal(err)
		}
		if output.String() != `content` || report.WarningCount() != 0 {
			t.Fatalf("write = %q, report %+v", output.String(), report)
		}
	}
}

func TestOpenFileRejectsDirectory(t *testing.T) {
	if _, err := OpenFile(fstest.MapFS{`dir/file`: {}}, `dir`); err == nil {
		t.Fatal("OpenFile accepted a directory")
	}
}

func TestReadAllLimit(t *testing.T) {
	source, err := OpenFile(fstest.MapFS{`file`: {Data: []byte(`content`)}}, `file`)
	if err != nil {
		t.Fatal(err)
	}
	data, _, err := ReadAll(context.Background(), source, 7)
	if err != nil || string(data) != `content` {
		t.Fatalf("exact limit = %q, %v", data, err)
	}
	if _, _, err := ReadAll(context.Background(), source, 6); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversized error = %v", err)
	}
}

func TestReadAllBoundsUnknownSize(t *testing.T) {
	source := testSource{write: func(_ context.Context, dst io.Writer) (Report, error) {
		_, err := dst.Write([]byte(`content`))
		return Report{}, err
	}}
	if _, _, err := ReadAll(context.Background(), source, 6); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("streaming oversized error = %v", err)
	}
}

func TestFileSourceHonorsCancellation(t *testing.T) {
	source, err := OpenFile(fstest.MapFS{`file`: {Data: []byte(`content`)}}, `file`)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := source.WriteTo(ctx, io.Discard); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled write error = %v", err)
	}
}

type testSource struct {
	write func(context.Context, io.Writer) (Report, error)
}

func (testSource) Filename() string    { return `test` }
func (testSource) Size() (int64, bool) { return 0, false }
func (s testSource) WriteTo(ctx context.Context, dst io.Writer) (Report, error) {
	return s.write(ctx, dst)
}
