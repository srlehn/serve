//go:build unix

package unixarchive

import (
	"io/fs"
	"syscall"
)

type fileKey struct{ device, inode uint64 }

func fileIdentity(info fs.FileInfo) (fileKey, uint64, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fileKey{}, 0, false
	}
	return fileKey{uint64(stat.Dev), uint64(stat.Ino)}, uint64(stat.Nlink), true
}
