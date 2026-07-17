//go:build aix || dragonfly || openbsd || solaris

package unixarchive

func readExtendedAttributes(string) ([]extendedAttribute, []error) { return nil, nil }
