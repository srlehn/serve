//go:build linux || darwin

package unixarchive

import (
	"errors"
	"fmt"
	"strings"

	"golang.org/x/sys/unix"
)

func readExtendedAttributes(filename string) ([]extendedAttribute, []error) {
	size, err := unix.Llistxattr(filename, nil)
	if err != nil {
		if unsupportedMetadata(err) {
			return nil, nil
		}
		return nil, []error{fmt.Errorf("extended attribute list failed: %w", err)}
	}
	if size == 0 {
		return nil, nil
	}
	if size > maxXattrSize {
		return nil, []error{fmt.Errorf("extended attribute list is %d bytes", size)}
	}
	buffer := make([]byte, size)
	size, err = unix.Llistxattr(filename, buffer)
	if err != nil {
		return nil, []error{fmt.Errorf("extended attribute list failed: %w", err)}
	}
	buffer = buffer[:size]
	if len(buffer) == 0 || buffer[len(buffer)-1] != 0 {
		return nil, []error{errors.New(`malformed extended attribute list`)}
	}
	names := strings.Split(string(buffer[:len(buffer)-1]), "\x00")
	attributes := make([]extendedAttribute, 0, len(names))
	var warnings []error
	for _, name := range names {
		value, err := readExtendedAttribute(filename, name)
		if err != nil {
			warnings = append(warnings, fmt.Errorf("extended attribute %q read failed: %w", name, err))
			continue
		}
		attributes = append(attributes, extendedAttribute{name, value})
	}
	return attributes, warnings
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
