package ghapp

import (
	"fmt"
	"log/slog"
	"unsafe"

	"golang.org/x/sys/windows"
)

// restrictToOwner replaces the file's DACL with a single entry granting the
// current user full control, and marks it protected so no inherited entry from
// the parent directory applies. Windows ignores the Unix mode passed to
// os.WriteFile/Chmod, so a credential written with 0600 is otherwise readable by
// every local account.
func restrictToOwner(path string) error {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return fmt.Errorf("current user sid: %w", err)
	}
	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.GENERIC_ALL,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       windows.NO_INHERITANCE,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_USER,
			TrusteeValue: windows.TrusteeValueFromSID(user.User.Sid),
		},
	}}, nil)
	if err != nil {
		return fmt.Errorf("build acl: %w", err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, acl, nil); err != nil {
		return fmt.Errorf("restrict %s to owner: %w", path, err)
	}
	return nil
}

// warnIfPermissive reports a credential file other accounts on this host can
// reach. Windows governs that by DACL rather than mode bits, so it reads the
// descriptor back rather than looking at permissions the filesystem ignores.
func warnIfPermissive(path string) {
	if err := CheckOwnerOnly(path); err != nil {
		slog.Warn("credential file is reachable by other accounts on this host; re-run `multirunner connect` to rewrite it",
			slog.String("path", path), slog.Any("error", err))
	}
}

// CheckOwnerOnly reports whether path is reachable only by the account running
// this process: a DACL protected against inheritance, carrying exactly one allow
// ACE that names the current user.
//
// It walks the ACEs rather than matching text in the descriptor's SDDL: SDDL
// abbreviates well-known SIDs to two-letter aliases, so a process running as the
// built-in Administrator - which is what CI does on Windows - renders its own
// entry as "LA" and never contains its SID as a string.
func CheckOwnerOnly(path string) error {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("read security info: %w", err)
	}
	control, _, err := sd.Control()
	if err != nil {
		return fmt.Errorf("read descriptor control: %w", err)
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		return fmt.Errorf("DACL is not protected, inherited entries still apply: %s", sd)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return fmt.Errorf("read dacl: %w", err)
	}
	if dacl == nil {
		return fmt.Errorf("no DACL, every account has full access: %s", sd)
	}
	if dacl.AceCount != 1 {
		return fmt.Errorf("DACL has %d ACEs, want exactly 1: %s", dacl.AceCount, sd)
	}

	var ace *windows.ACCESS_ALLOWED_ACE
	if err := windows.GetAce(dacl, 0, &ace); err != nil {
		return fmt.Errorf("read ace: %w", err)
	}
	if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
		return fmt.Errorf("ACE type = %d, want an allow ACE: %s", ace.Header.AceType, sd)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
	if !sid.Equals(user.User.Sid) {
		return fmt.Errorf("DACL names %s, want the current user %s", sid, user.User.Sid)
	}
	return nil
}
