//go:build !unix

package unixarchive

import "io/fs"

type fileKey struct{}

func fileIdentity(fs.FileInfo) (fileKey, uint64, bool) {
	return fileKey{}, 0, false
}
