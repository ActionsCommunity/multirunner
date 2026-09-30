package ghapp

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

// FILE_ALL_ACCESS from the Windows SDK (standard rights, synchronize, and
// all nine file-specific rights); SetNamedSecurityInfo may map GENERIC_ALL.
const fileAllAccess = windows.STANDARD_RIGHTS_REQUIRED | windows.SYNCHRONIZE | 0x1ff

// restrictToOwner grants access to the connecting user and LocalSystem, the
// default Windows service identity, without inheriting directory permissions.
func restrictToOwner(path string) error {
	return restrictToOwnerFrom(path, "")
}

func restrictToOwnerFrom(path, previousPath string) error {
	if previousPath != "" {
		if _, err := os.Stat(previousPath); errors.Is(err, os.ErrNotExist) {
			previousPath = ""
		} else if err != nil {
			return err
		}
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return fmt.Errorf("current user sid: %w", err)
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return err
	}
	var acl *windows.ACL
	// A service refresh must retain the operator's ACE. Copy only a validated
	// credential ACL; never preserve inherited or otherwise permissive access.
	if previousPath != "" && user.User.Sid.Equals(system) {
		if err := CheckOwnerOnly(previousPath); err != nil {
			return err
		}
		sd, err := windows.GetNamedSecurityInfo(previousPath, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
		if err != nil {
			return err
		}
		acl, _, err = sd.DACL()
		if err != nil {
			return err
		}
		// sd owns acl's backing memory; retain it until SetNamedSecurityInfo returns.
		defer runtime.KeepAlive(sd)
	} else {
		sids := []*windows.SID{user.User.Sid}
		if !user.User.Sid.Equals(system) {
			sids = append(sids, system)
		}
		entries := make([]windows.EXPLICIT_ACCESS, 0, len(sids))
		for _, sid := range sids {
			entries = append(entries, windows.EXPLICIT_ACCESS{
				AccessPermissions: windows.GENERIC_ALL,
				AccessMode:        windows.GRANT_ACCESS,
				Inheritance:       windows.NO_INHERITANCE,
				Trustee:           windows.TRUSTEE{TrusteeForm: windows.TRUSTEE_IS_SID, TrusteeType: windows.TRUSTEE_IS_USER, TrusteeValue: windows.TrusteeValueFromSID(sid)},
			})
		}
		acl, err = windows.ACLFromEntries(entries, nil)
		if err != nil {
			return fmt.Errorf("build acl: %w", err)
		}
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, acl, nil); err != nil {
		return fmt.Errorf("restrict %s: %w", path, err)
	}
	return nil
}

// warnIfPermissive reports a credential file other accounts on this host can
// reach. Windows governs that by DACL rather than mode bits, so it reads the
// descriptor back rather than looking at permissions the filesystem ignores.
func warnIfPermissive(path string) {
	if err := CheckOwnerOnly(path); err != nil {
		slog.Warn("credential file has unexpected access permissions; re-run `multirunner connect` to rewrite it",
			slog.String("path", path), slog.Any("error", err))
	}
}

// CheckOwnerOnly verifies a protected credential DACL: LocalSystem and at
// most one operator account, both with full control. When running as the
// operator, its SID must be that account; LocalSystem accepts the preserved
// operator ACE so rotating the file never removes interactive access.
func CheckOwnerOnly(path string) error {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("read security info: %w", err)
	}
	control, _, err := sd.Control()
	if err != nil {
		return err
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		return fmt.Errorf("DACL is not protected: %s", sd)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	if dacl == nil || dacl.AceCount < 1 || dacl.AceCount > 2 {
		return fmt.Errorf("unexpected credential DACL: %s", sd)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return err
	}
	systemFound, operatorFound := false, false
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return err
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || ace.Header.AceFlags != 0 ||
			(ace.Mask != windows.GENERIC_ALL && ace.Mask != fileAllAccess) {
			return fmt.Errorf("unexpected credential ACE: %s", sd)
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if sid.Equals(system) {
			if systemFound {
				return fmt.Errorf("duplicate LocalSystem ACE")
			}
			systemFound = true
		} else {
			if operatorFound || (!user.User.Sid.Equals(system) && !sid.Equals(user.User.Sid)) {
				return fmt.Errorf("unexpected credential account: %s", sd)
			}
			if user.User.Sid.Equals(system) {
				// An arbitrary group (for example Everyone) is not an operator.
				_, _, accountType, err := sid.LookupAccount("")
				if err != nil || accountType != windows.SidTypeUser {
					return fmt.Errorf("credential ACE does not name an operator user: %s", sd)
				}
			}
			operatorFound = true
		}
	}
	if !systemFound || (!user.User.Sid.Equals(system) && !operatorFound) {
		return fmt.Errorf("missing credential account: %s", sd)
	}
	return nil
}
