//go:build unix

package unixarchive

import (
	"archive/tar"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

const maxXattrSize = 1 << 20

type extendedAttribute struct {
	name  string
	value []byte
}

type unixMetadata struct{ root *os.Root }

func metadataFor(root *os.Root) metadataReader { return unixMetadata{root} }

func (m unixMetadata) enrich(name string, info fs.FileInfo, header *tar.Header) []error {
	var filename string
	var warnings []error
	if m.root != nil {
		filename = filepath.Join(m.root.Name(), filepath.FromSlash(name))
		attributes, attributeWarnings := readExtendedAttributes(filename)
		warnings = append(warnings, attributeWarnings...)
		for _, attribute := range attributes {
			if strings.ContainsRune(attribute.name, '=') {
				warnings = append(warnings, fmt.Errorf("extended attribute %q is not representable as a PAX key", attribute.name))
				continue
			}
			pax(header)[`SCHILY.xattr.`+attribute.name] = string(attribute.value)
			key, value, err := aclPAX(attribute.name, attribute.value)
			if err != nil {
				warnings = append(warnings, fmt.Errorf("ACL conversion failed: %w", err))
				continue
			}
			if key != `` {
				pax(header)[key] = value
			}
		}
	}
	return append(warnings, platformMetadata(m.root, name, filename, info, header)...)
}

func pax(header *tar.Header) map[string]string {
	if header.PAXRecords == nil {
		header.PAXRecords = make(map[string]string)
	}
	return header.PAXRecords
}

func unsupportedMetadata(err error) bool {
	return errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP) ||
		errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.ENOTTY) || errors.Is(err, unix.EINVAL)
}

func paxTime(seconds, nanoseconds int64) string {
	value := strconv.FormatInt(seconds, 10)
	if nanoseconds == 0 {
		return value
	}
	return value + `.` + strings.TrimRight(fmt.Sprintf("%09d", nanoseconds), `0`)
}
