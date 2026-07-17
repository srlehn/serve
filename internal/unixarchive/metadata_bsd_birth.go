//go:build darwin || freebsd || netbsd

package unixarchive

import "syscall"

func bsdCreationTime(stat *syscall.Stat_t) (int64, int64, bool) {
	seconds := int64(stat.Birthtimespec.Sec)
	nanoseconds := int64(stat.Birthtimespec.Nsec)
	return seconds, nanoseconds, seconds != 0 || nanoseconds != 0
}
