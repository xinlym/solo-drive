//go:build linux

package drive

import (
	"fmt"
	"math"
	"syscall"
)

// diskCapacity reports the filesystem containing path. BAVAIL excludes the
// filesystem's own reserved blocks, even when this process is running as root.
func diskCapacity(path string) (total, used, available int64, err error) {
	var s syscall.Statfs_t
	if err = syscall.Statfs(path, &s); err != nil {
		return
	}
	if s.Bsize <= 0 {
		err = fmt.Errorf("invalid filesystem block size")
		return
	}
	block := uint64(s.Bsize)
	product := func(n uint64) int64 {
		if n > uint64(math.MaxInt64)/block {
			return math.MaxInt64
		}
		return int64(n * block)
	}
	total = product(s.Blocks)
	available = product(s.Bavail)
	if s.Blocks >= s.Bfree {
		used = product(s.Blocks - s.Bfree)
	}
	return
}
