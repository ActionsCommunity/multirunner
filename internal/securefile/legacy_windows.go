//go:build windows

package securefile

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

// repairLegacyDACL upgrades a secret written by releases that relied on the
// protected parent directory instead of protecting the file's own DACL. The
// file is only tightened, never widened, and only when all of these hold:
//
//   - the caller's installation identity is Administrators (elevated or
//     LocalSystem), so the result is the DACL the service expects;
//   - the parent directory's DACL is protected, so the file's access came from
//     an administrator-chosen directory ACL rather than an ordinary location;
//   - every file ACE is an inherited allow entry for LocalSystem,
//     Administrators, or an individual user account, never a group.
//
// Any other state is left unchanged so validation still rejects it.
func repairLegacyDACL(path string) error {
	system, installation, _, administrators, err := expectedSIDs()
	if err != nil {
		return err
	}
	if !installation.Equals(administrators) {
		return nil
	}
	protected, err := directoryDACLProtected(filepath.Dir(path))
	if err != nil || !protected {
		return nil
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	handle, err := windows.CreateFile(
		name,
		windows.READ_CONTROL|windows.WRITE_DAC|windows.WRITE_OWNER,
		windows.FILE_SHARE_READ,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		// Validation reports the real problem when the file cannot be opened.
		return nil
	}
	defer windows.CloseHandle(handle)

	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return nil
	}
	if info.FileAttributes&(windows.FILE_ATTRIBUTE_DIRECTORY|windows.FILE_ATTRIBUTE_REPARSE_POINT) != 0 ||
		info.NumberOfLinks != 1 {
		return nil
	}
	sd, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return nil
	}
	if !legacyDACLRepairable(sd, system, administrators) {
		return nil
	}

	acl, err := protectedDACL(system, installation)
	if err != nil {
		return err
	}
	if err := windows.SetSecurityInfo(
		handle, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		installation, nil, acl, nil,
	); err != nil {
		return fmt.Errorf("protect legacy secret DACL: %w", err)
	}
	return nil
}

func legacyDACLRepairable(sd *windows.SECURITY_DESCRIPTOR, system, administrators *windows.SID) bool {
	control, _, err := sd.Control()
	if err != nil || control&windows.SE_DACL_PROTECTED != 0 {
		return false
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil || dacl.AceCount == 0 {
		return false
	}
	for index := uint32(0); index < uint32(dacl.AceCount); index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, index, &ace); err != nil {
			return false
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE ||
			ace.Header.AceFlags&windows.INHERITED_ACE == 0 {
			return false
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if sid.Equals(system) || sid.Equals(administrators) {
			continue
		}
		_, _, use, err := sid.LookupAccount("")
		if err != nil || use != windows.SidTypeUser {
			return false
		}
	}
	runtime.KeepAlive(sd)
	return true
}

func directoryDACLProtected(directory string) (bool, error) {
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() {
		return false, err
	}
	sd, err := windows.GetNamedSecurityInfo(directory, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return false, err
	}
	control, _, err := sd.Control()
	if err != nil {
		return false, err
	}
	return control&windows.SE_DACL_PROTECTED != 0, nil
}
