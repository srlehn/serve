//go:build !unix

package unixarchive

import "os"

func metadataFor(*os.Root) metadataReader { return nil }
