//go:build !windows

package backup

import "golang.org/x/sys/unix"

func diskFreeSpace(path string) (uint64, uint64, error) {
	var stats unix.Statfs_t
	if err := unix.Statfs(path, &stats); err != nil {
		return 0, 0, err
	}
	blockSize := uint64(stats.Bsize)
	return uint64(stats.Bavail) * blockSize, uint64(stats.Blocks) * blockSize, nil
}
