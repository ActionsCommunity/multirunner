//go:build windows

package securefile

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

const fileAllAccess = windows.STANDARD_RIGHTS_REQUIRED | windows.SYNCHRONIZE | 0x1ff

const trustedInstallerSID = "S-1-5-80-956008885-3418522649-1831038044-1853292631-2271478464"

func CreateExclusive(path string, data []byte) error {
	if err := validateParentChain(path); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		_ = file.Close()
		if !ok {
			_ = os.Remove(path)
		}
	}()
	if err := applyProtectedDACL(file); err != nil {
		return err
	}
	if err := validateOpenFile(file); err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		return fmt.Errorf("write secret file: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync secret file: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close secret file: %w", err)
	}
	ok = true
	return nil
}

func Replace(path string, data []byte) error {
	if err := Check(path); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".securefile-*.tmp")
	if err != nil {
		return fmt.Errorf("create replacement secret: %w", err)
	}
	tempPath := file.Name()
	ok := false
	defer func() {
		_ = file.Close()
		if !ok {
			_ = os.Remove(tempPath)
		}
	}()
	if err := applyProtectedDACL(file); err != nil {
		return err
	}
	if err := validateOpenFile(file); err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		return fmt.Errorf("write replacement secret: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync replacement secret: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close replacement secret: %w", err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("replace secret file: %w", err)
	}
	ok = true
	return nil
}

func Read(path string) ([]byte, error) {
	if err := validateParentChain(path); err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	if err := validateOpenFile(file); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return nil, fmt.Errorf("read secret file: %w", err)
	}
	return data, nil
}

func Check(path string) error {
	if err := validateParentChain(path); err != nil {
		return err
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	return validateOpenFile(file)
}

func validateOpenFile(file *os.File) error {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(file.Fd()), &info); err != nil {
		return err
	}
	if info.FileAttributes&(windows.FILE_ATTRIBUTE_DIRECTORY|windows.FILE_ATTRIBUTE_REPARSE_POINT) != 0 {
		return fmt.Errorf("%w: secret is not a plain file", ErrInsecurePermissions)
	}
	err := validateOpenHandle(windows.Handle(file.Fd()))
	return err
}

func validateOpenHandle(handle windows.Handle) error {
	sd, err := windows.GetSecurityInfo(
		handle, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION,
	)
	if err != nil {
		return fmt.Errorf("read secret security info: %w", err)
	}
	control, _, err := sd.Control()
	if err != nil {
		return err
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		return fmt.Errorf("%w: DACL is not protected", ErrInsecurePermissions)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	if dacl == nil || dacl.AceCount != 2 {
		return fmt.Errorf("%w: unexpected DACL", ErrInsecurePermissions)
	}
	system, installation, current, administrators, err := expectedSIDs()
	if err != nil {
		return err
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return err
	}
	if owner == nil || (!owner.Equals(system) && !owner.Equals(administrators) &&
		(current.Equals(system) || !owner.Equals(current))) {
		return fmt.Errorf("%w: owner SID does not match an expected service or installation identity", ErrInsecurePermissions)
	}
	systemFound := false
	installationFound := false
	for index := uint32(0); index < uint32(dacl.AceCount); index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, index, &ace); err != nil {
			return err
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || ace.Header.AceFlags != 0 ||
			(ace.Mask != windows.GENERIC_ALL && ace.Mask != fileAllAccess) {
			return fmt.Errorf("%w: unexpected access entry", ErrInsecurePermissions)
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if sid.Equals(system) {
			if systemFound {
				return fmt.Errorf("%w: duplicate LocalSystem access entry", ErrInsecurePermissions)
			}
			systemFound = true
			continue
		}
		if sid.Equals(installation) || sid.Equals(administrators) ||
			(!current.Equals(system) && sid.Equals(current)) {
			if installationFound {
				return fmt.Errorf("%w: duplicate installation identity access entry", ErrInsecurePermissions)
			}
			installationFound = true
			continue
		}
		return fmt.Errorf("%w: unexpected account access entry", ErrInsecurePermissions)
	}
	if !systemFound || !installationFound {
		return fmt.Errorf("%w: required account access is missing", ErrInsecurePermissions)
	}
	runtime.KeepAlive(sd)
	return nil
}

func applyProtectedDACL(file *os.File) error {
	system, installation, _, _, err := expectedSIDs()
	if err != nil {
		return err
	}
	sids := []*windows.SID{system, installation}
	entries := make([]windows.EXPLICIT_ACCESS, 0, len(sids))
	for _, sid := range sids {
		entries = append(entries, windows.EXPLICIT_ACCESS{
			AccessPermissions: windows.GENERIC_ALL,
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       windows.NO_INHERITANCE,
			Trustee: windows.TRUSTEE{
				TrusteeForm: windows.TRUSTEE_IS_SID, TrusteeType: windows.TRUSTEE_IS_USER,
				TrusteeValue: windows.TrusteeValueFromSID(sid),
			},
		})
	}
	acl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		return fmt.Errorf("build secret DACL: %w", err)
	}
	if err := windows.SetNamedSecurityInfo(
		file.Name(), windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		installation, nil, acl, nil,
	); err != nil {
		return fmt.Errorf("protect secret DACL: %w", err)
	}
	return nil
}

func expectedSIDs() (*windows.SID, *windows.SID, *windows.SID, *windows.SID, error) {
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	current, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, nil, nil, nil, err
	}
	administrators, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	installation := current.User.Sid
	if installation.Equals(system) || windows.GetCurrentProcessToken().IsElevated() {
		installation = administrators
	}
	return system, installation, current.User.Sid, administrators, nil
}

func validateParentChain(path string) error {
	parent, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return err
	}
	system, installation, current, administrators, err := expectedSIDs()
	if err != nil {
		return err
	}
	trustedInstaller, err := windows.StringToSid(trustedInstallerSID)
	if err != nil {
		return err
	}
	for {
		name, err := windows.UTF16PtrFromString(parent)
		if err != nil {
			return err
		}
		handle, err := windows.CreateFile(
			name,
			windows.READ_CONTROL,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
			nil,
			windows.OPEN_EXISTING,
			windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT,
			0,
		)
		if err != nil {
			return err
		}
		var info windows.ByHandleFileInformation
		if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
			windows.CloseHandle(handle)
			return err
		}
		if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 ||
			info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
			windows.CloseHandle(handle)
			return fmt.Errorf("%w: parent path %q is not a plain directory", ErrInsecurePermissions, parent)
		}
		sd, err := windows.GetSecurityInfo(
			handle, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION,
		)
		windows.CloseHandle(handle)
		if err != nil {
			return err
		}
		owner, _, err := sd.Owner()
		if err != nil {
			return err
		}
		if owner == nil ||
			(!owner.Equals(system) && !owner.Equals(installation) &&
				!owner.Equals(current) && !owner.Equals(administrators) &&
				!owner.Equals(trustedInstaller)) {
			return fmt.Errorf("%w: parent path %q has an unexpected owner SID", ErrInsecurePermissions, parent)
		}
		runtime.KeepAlive(sd)
		next := filepath.Dir(parent)
		if next == parent {
			return nil
		}
		parent = next
	}
}
