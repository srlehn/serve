//go:build linux

package unixarchive

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestArchiveLinuxXattr(t *testing.T) {
	directory := t.TempDir()
	if err := os.Mkdir(filepath.Join(directory, "tree"), 0o700); err != nil {
		t.Fatal(err)
	}
	filename := filepath.Join(directory, "tree", "file")
	if err := os.WriteFile(filename, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	value := []byte{'a', 0, 'b'}
	if err := unix.Lsetxattr(filename, "user.serve-test", value, 0); err != nil {
		if unsupportedMetadata(err) || errors.Is(err, unix.EPERM) {
			t.Skip(err)
		}
		t.Fatal(err)
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	source, err := New(root.FS(), "tree", Options{OSRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	members, report := unpack(t, source)
	if report.WarningCount() != 0 {
		t.Fatalf("unexpected warnings: %+v", report)
	}
	got := members["tree/file"].header.PAXRecords["SCHILY.xattr.user.serve-test"]
	if got != string(value) {
		t.Fatalf("xattr = %q", got)
	}
}

func TestFormatACL(t *testing.T) {
	value := make([]byte, 4)
	binary.LittleEndian.PutUint32(value, 2)
	for _, entry := range []struct {
		tag, permissions uint16
		id               uint32
	}{{1, 7, 0}, {2, 5, 123}, {4, 4, 0}, {16, 5, 0}, {32, 1, 0}} {
		data := make([]byte, 8)
		binary.LittleEndian.PutUint16(data, entry.tag)
		binary.LittleEndian.PutUint16(data[2:], entry.permissions)
		binary.LittleEndian.PutUint32(data[4:], entry.id)
		value = append(value, data...)
	}
	got, err := formatACL(value)
	if err != nil {
		t.Fatal(err)
	}
	if want := "user::rwx,user:123:r-x,group::r--,mask::r-x,other::--x"; got != want {
		t.Fatalf("ACL = %q, want %q", got, want)
	}
}
