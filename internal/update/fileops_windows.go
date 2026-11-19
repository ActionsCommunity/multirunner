//go:build windows

package update

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

func replaceFile(source, destination string) error {
	sourcePath, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	destinationPath, err := windows.UTF16PtrFromString(destination)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(
		sourcePath,
		destinationPath,
		windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH,
	)
}

func syncDirectory(string) error {
	return nil
}

func openArtifactFile(path string) (*os.File, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(
		name,
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(handle), path), nil
}

func validateRegularFile(file *os.File, info os.FileInfo) error {
	if !info.Mode().IsRegular() {
		return ErrIntegrity
	}
	var details windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(file.Fd()), &details); err != nil {
		return err
	}
	if details.NumberOfLinks != 1 {
		return fmt.Errorf("%w: update artifact must have exactly one link", ErrIntegrity)
	}
	return nil
}
