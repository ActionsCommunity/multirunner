package ghapp

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// checkOwnerOnly asserts that the DACL is protected against inheritance and
// carries exactly one allow ACE, naming the current user.
//
// It walks the ACEs rather than matching text in the descriptor's SDDL: SDDL
// abbreviates well-known SIDs to two-letter aliases, so a process running as the
// built-in Administrator - which is what CI does on Windows - renders its own
// entry as "LA" and never contains its SID as a string.
func checkOwnerOnly(path string) error {
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
