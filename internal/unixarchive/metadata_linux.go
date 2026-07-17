//go:build linux

package unixarchive

import (
	"archive/tar"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

func platformMetadata(root *os.Root, name, filename string, info fs.FileInfo, header *tar.Header) []error {
	if root == nil {
		return nil
	}
	var warnings []error
	var stat unix.Statx_t
	if err := unix.Statx(unix.AT_FDCWD, filename, unix.AT_SYMLINK_NOFOLLOW, unix.STATX_BTIME, &stat); err == nil && stat.Mask&unix.STATX_BTIME != 0 {
		pax(header)[`LIBARCHIVE.creationtime`] = paxTime(stat.Btime.Sec, int64(stat.Btime.Nsec))
	} else if err != nil && !unsupportedMetadata(err) {
		warnings = append(warnings, fmt.Errorf("creation time read failed: %w", err))
	}

	if info.Mode().IsRegular() || info.IsDir() {
		file, err := root.Open(name)
		if err != nil {
			warnings = append(warnings, fmt.Errorf("file flags open failed: %w", err))
		} else {
			flags, flagErr := unix.IoctlGetInt(int(file.Fd()), unix.FS_IOC_GETFLAGS)
			if flagErr == nil {
				if text := fileFlags(uint32(flags)); text != `` {
					pax(header)[`SCHILY.fflags`] = text
				}
			} else if !unsupportedMetadata(flagErr) {
				warnings = append(warnings, fmt.Errorf("file flags read failed: %w", flagErr))
			}
			if err := file.Close(); err != nil {
				warnings = append(warnings, fmt.Errorf("file flags close failed: %w", err))
			}
		}
	}
	return warnings
}

func aclPAX(name string, value []byte) (string, string, error) {
	if name != `system.posix_acl_access` && name != `system.posix_acl_default` {
		return ``, ``, nil
	}
	acl, err := formatACL(value)
	key := `SCHILY.acl.access`
	if name == `system.posix_acl_default` {
		key = `SCHILY.acl.default`
	}
	return key, acl, err
}

var knownFileFlags = []struct {
	bit  uint32
	name string
}{
	{0x00000001, `secdel`},
	{0x00000002, `undel`},
	{0x00000004, `compress`},
	{0x00000008, `sync`},
	{0x00000010, `schg`},
	{0x00000020, `sappnd`},
	{0x00000040, `nodump`},
	{0x00000080, `noatime`},
	{0x00004000, `journal-data`},
	{0x00008000, `notail`},
	{0x00010000, `dirsync`},
	{0x00020000, `topdir`},
	{0x00800000, `nocow`},
	{0x20000000, `projinherit`},
}

func fileFlags(flags uint32) string {
	var names []string
	for _, flag := range knownFileFlags {
		if flags&flag.bit != 0 {
			names = append(names, flag.name)
		}
	}
	return strings.Join(names, `,`)
}

func formatACL(value []byte) (string, error) {
	if len(value) < 4 || (len(value)-4)%8 != 0 || binary.LittleEndian.Uint32(value) != 2 {
		return ``, errors.New(`invalid Linux ACL encoding`)
	}
	var entries []string
	for offset := 4; offset < len(value); offset += 8 {
		tag := binary.LittleEndian.Uint16(value[offset:])
		permissions := binary.LittleEndian.Uint16(value[offset+2:])
		id := binary.LittleEndian.Uint32(value[offset+4:])
		if permissions > 7 {
			return ``, errors.New(`invalid Linux ACL permissions`)
		}
		perm := []byte(`---`)
		if permissions&4 != 0 {
			perm[0] = 'r'
		}
		if permissions&2 != 0 {
			perm[1] = 'w'
		}
		if permissions&1 != 0 {
			perm[2] = 'x'
		}
		prefix := map[uint16]string{1: `user::`, 2: `user:` + strconv.FormatUint(uint64(id), 10) + `:`, 4: `group::`, 8: `group:` + strconv.FormatUint(uint64(id), 10) + `:`, 16: `mask::`, 32: `other::`}[tag]
		if prefix == `` {
			return ``, fmt.Errorf("unknown Linux ACL tag %#x", tag)
		}
		entries = append(entries, prefix+string(perm))
	}
	return strings.Join(entries, `,`), nil
}
