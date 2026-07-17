//go:build unix && !linux

package unixarchive

func aclPAX(string, []byte) (string, string, error) { return ``, ``, nil }
