//go:build windows

package backup

import (
	"path/filepath"

	"golang.org/x/sys/windows"
)

func diskFreeSpace(path string) (uint64, uint64, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return 0, 0, err
	}
	pointer, err := windows.UTF16PtrFromString(absolute)
	if err != nil {
		return 0, 0, err
	}
	var available, total, free uint64
	err = windows.GetDiskFreeSpaceEx(pointer, &available, &total, &free)
	return available, total, err
}
