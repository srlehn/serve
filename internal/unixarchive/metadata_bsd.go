//go:build darwin || dragonfly || freebsd || netbsd || openbsd

package unixarchive

import (
	"archive/tar"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"syscall"
)

func platformMetadata(_ *os.Root, _, _ string, info fs.FileInfo, header *tar.Header) []error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	seconds, nanoseconds, hasCreationTime := bsdCreationTime(stat)
	if hasCreationTime {
		pax(header)[`LIBARCHIVE.creationtime`] = paxTime(seconds, nanoseconds)
	}
	flags, unknown := bsdFileFlags(stat.Flags)
	if flags != `` {
		pax(header)[`SCHILY.fflags`] = flags
	}
	if unknown != 0 {
		return []error{fmt.Errorf("file flags %#x are not representable", unknown)}
	}
	return nil
}

var knownBSDFileFlags = []struct {
	bit  uint32
	name string
}{
	{0x00000001, `nodump`},
	{0x00000002, `uchg`},
	{0x00000004, `uappnd`},
	{0x00000008, `opaque`},
	{0x00000010, `uunlnk`},
	{0x00010000, `arch`},
	{0x00020000, `schg`},
	{0x00040000, `sappnd`},
	{0x00100000, `sunlnk`},
}

func bsdFileFlags(flags uint32) (string, uint32) {
	remaining := flags
	var names []string
	for _, flag := range knownBSDFileFlags {
		if flags&flag.bit != 0 {
			names = append(names, flag.name)
			remaining &^= flag.bit
		}
	}
	return strings.Join(names, `,`), remaining
}
