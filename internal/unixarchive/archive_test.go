package unixarchive

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/srlehn/serve/internal/payload"
)

func TestArchiveTree(t *testing.T) {
	modified := time.Date(2026, 7, 17, 10, 20, 30, 123456789, time.UTC)
	files := fstest.MapFS{
		"tree":                  {Mode: fs.ModeDir | 0o750, ModTime: modified},
		"tree/empty":            {Mode: fs.ModeDir | 0o700, ModTime: modified},
		"tree/nested/grüße.txt": {Data: []byte("hello\n"), Mode: 0o640, ModTime: modified},
	}
	source, err := New(files, "tree", Options{RootName: "bundle"})
	if err != nil {
		t.Fatal(err)
	}
	if got := source.Filename(); got != "bundle.tar.zst" {
		t.Fatalf("filename = %q", got)
	}
	members, report := unpack(t, source)
	if report.WarningCount() != 0 {
		t.Fatalf("unexpected warnings: %+v", report)
	}
	for _, name := range []string{"bundle/", "bundle/empty/", "bundle/nested/", "bundle/nested/grüße.txt"} {
		if _, ok := members[name]; !ok {
			t.Errorf("missing member %q", name)
		}
	}
	file := members["bundle/nested/grüße.txt"]
	if string(file.data) != "hello\n" || file.header.Mode&0o777 != 0o640 || !file.header.ModTime.Equal(modified) {
		t.Fatalf("file was not preserved: header=%+v data=%q", file.header, file.data)
	}
}

func TestArchiveContinuesAfterOpenFailure(t *testing.T) {
	files := openErrorFS{FS: fstest.MapFS{
		"tree/bad":                 {Data: []byte("secret")},
		"tree/bad.serve-error.txt": {Data: []byte("real file")},
	}, target: "tree/bad"}
	source, err := New(files, "tree", Options{RootName: "bundle"})
	if err != nil {
		t.Fatal(err)
	}
	if got := source.Filename(); got != "bundle.incomplete.tar.zst" {
		t.Fatalf("filename = %q", got)
	}
	members, report := unpack(t, source)
	if report.WarningCount() != 1 {
		t.Fatalf("warnings = %+v", report)
	}
	if _, ok := members["bundle/bad"]; ok {
		t.Fatal("unreadable file was included")
	}
	marker := members["bundle/bad.serve-error.1.txt"]
	if !strings.Contains(string(marker.data), "file omitted") {
		t.Fatalf("bad error marker: %q", marker.data)
	}
	if _, ok := members["bundle/serve-archive-errors.txt"]; !ok {
		t.Fatal("aggregate error report is missing")
	}
}

func TestArchiveHonorsCancellation(t *testing.T) {
	source, err := New(fstest.MapFS{"tree/file": {Data: []byte("data")}}, "tree", Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := source.WriteTo(ctx, io.Discard); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
}

type archivedMember struct {
	header tar.Header
	data   []byte
}

func unpack(t *testing.T, source *Source) (map[string]archivedMember, payload.Report) {
	t.Helper()
	var compressed bytes.Buffer
	report, err := source.WriteTo(context.Background(), &compressed)
	if err != nil {
		t.Fatal(err)
	}
	decoder, err := zstd.NewReader(bytes.NewReader(compressed.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	defer decoder.Close()
	reader := tar.NewReader(decoder)
	members := make(map[string]archivedMember)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		members[header.Name] = archivedMember{header: *header, data: data}
	}
	return members, report
}

type openErrorFS struct {
	fs.FS
	target string
}

func (f openErrorFS) Open(name string) (fs.File, error) {
	if name == f.target {
		return nil, fs.ErrPermission
	}
	return f.FS.Open(name)
}
