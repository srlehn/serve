//go:build unix

package unixarchive

import (
	"archive/tar"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestArchiveUnixEntries(t *testing.T) {
	directory := t.TempDir()
	tree := filepath.Join(directory, "tree")
	if err := os.Mkdir(tree, 0o750); err != nil {
		t.Fatal(err)
	}
	original := filepath.Join(tree, "a-original")
	if err := os.WriteFile(original, []byte("linked"), 0o651); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(original, filepath.Join(tree, "b-hardlink")); err != nil {
		t.Skip(err)
	}
	if err := os.Symlink("a-original", filepath.Join(tree, "link")); err != nil {
		t.Skip(err)
	}
	if err := syscall.Mkfifo(filepath.Join(tree, "pipe"), 0o620); err != nil {
		t.Skip(err)
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	source, err := New(root.FS(), "tree", Options{RootName: "bundle", OSRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	members, report := unpack(t, source)
	if report.WarningCount() != 0 {
		t.Fatalf("unexpected warnings: %+v", report)
	}
	if header := members["bundle/link"].header; header.Typeflag != tar.TypeSymlink || header.Linkname != "a-original" {
		t.Fatalf("bad symlink header: %+v", header)
	}
	if header := members["bundle/b-hardlink"].header; header.Typeflag != tar.TypeLink || header.Linkname != "bundle/a-original" {
		t.Fatalf("bad hardlink header: %+v", header)
	}
	if header := members["bundle/pipe"].header; header.Typeflag != tar.TypeFifo {
		t.Fatalf("bad FIFO header: %+v", header)
	}
	var stat syscall.Stat_t
	if err := syscall.Lstat(original, &stat); err != nil {
		t.Fatal(err)
	}
	header := members["bundle/a-original"].header
	if header.Uid != int(stat.Uid) || header.Gid != int(stat.Gid) || header.AccessTime.IsZero() || header.ChangeTime.IsZero() {
		t.Fatalf("Unix metadata was not preserved: %+v", header)
	}
}
