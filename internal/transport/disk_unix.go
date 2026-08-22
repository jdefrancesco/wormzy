//go:build !windows

package transport

import (
	"fmt"
	"math"
	"syscall"
)

// diskFreeBytes reports the bytes available to an unprivileged process at path.
func diskFreeBytes(path string) (uint64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, err
	}
	return diskFreeBytesFromStat(uint64(stat.Bavail), int64(stat.Bsize))
}

// diskFreeBytesFromStat validates filesystem counters before calculating bytes.
func diskFreeBytesFromStat(availableBlocks uint64, blockSize int64) (uint64, error) {
	if blockSize <= 0 {
		return 0, fmt.Errorf("filesystem reported invalid block size %d", blockSize)
	}
	unsignedBlockSize := uint64(blockSize)
	if availableBlocks > math.MaxUint64/unsignedBlockSize {
		return 0, fmt.Errorf("filesystem free-space value overflows uint64")
	}
	return availableBlocks * unsignedBlockSize, nil
}
