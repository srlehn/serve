//go:build dragonfly || openbsd

package unixarchive

import "syscall"

func bsdCreationTime(*syscall.Stat_t) (int64, int64, bool) { return 0, 0, false }
