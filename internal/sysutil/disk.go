//go:build !windows

package sysutil

import (
	"fmt"
	"syscall"
)

// DiskFreeGB returns free disk space in GB for the given path.
func DiskFreeGB(path string) (float64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, fmt.Errorf("statfs %s: %w", path, err)
	}
	free := stat.Bavail * uint64(stat.Bsize)
	return float64(free) / (1024 * 1024 * 1024), nil
}