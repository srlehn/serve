//go:build freebsd || netbsd

package unixarchive

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

func readExtendedAttributes(filename string) ([]extendedAttribute, []error) {
	var attributes []extendedAttribute
	var warnings []error
	for _, namespace := range []struct {
		id     int
		prefix string
	}{{unix.EXTATTR_NAMESPACE_USER, `user.`}, {unix.EXTATTR_NAMESPACE_SYSTEM, `system.`}} {
		names, err := extattrNames(filename, namespace.id)
		if err != nil {
			if !unsupportedMetadata(err) && !errors.Is(err, unix.EPERM) {
				warnings = append(warnings, fmt.Errorf("extended attribute list failed: %w", err))
			}
			continue
		}
		for _, name := range names {
			fullName := namespace.prefix + name
			value, err := readExtendedAttribute(filename, fullName)
			if err != nil {
				warnings = append(warnings, fmt.Errorf("extended attribute %q read failed: %w", fullName, err))
				continue
			}
			attributes = append(attributes, extendedAttribute{fullName, value})
		}
	}
	return attributes, warnings
}

func extattrNames(filename string, namespace int) ([]string, error) {
	size, err := unix.LlistxattrNS(filename, namespace, nil)
	if err != nil || size == 0 {
		return nil, err
	}
	if size > maxXattrSize {
		return nil, fmt.Errorf("extended attribute list is %d bytes", size)
	}
	buffer := make([]byte, size)
	size, err = unix.LlistxattrNS(filename, namespace, buffer)
	if err != nil {
		return nil, err
	}
	buffer = buffer[:size]
	var names []string
	for len(buffer) != 0 {
		length := int(buffer[0])
		buffer = buffer[1:]
		if length == 0 || length > len(buffer) {
			return nil, errors.New(`malformed extended attribute list`)
		}
		names = append(names, string(buffer[:length]))
		buffer = buffer[length:]
	}
	return names, nil
}

func readExtendedAttribute(filename, name string) ([]byte, error) {
	size, err := unix.Lgetxattr(filename, name, nil)
	if err != nil {
		return nil, err
	}
	if size > maxXattrSize {
		return nil, fmt.Errorf("value is %d bytes", size)
	}
	value := make([]byte, size)
	size, err = unix.Lgetxattr(filename, name, value)
	if err != nil {
		return nil, err
	}
	return value[:size], nil
}
