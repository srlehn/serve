//go:build aix || solaris

package unixarchive

import (
	"archive/tar"
	"io/fs"
	"os"
)

func platformMetadata(*os.Root, string, string, fs.FileInfo, *tar.Header) []error { return nil }
