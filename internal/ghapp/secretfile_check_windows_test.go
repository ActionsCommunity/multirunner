package ghapp

import (
	"fmt"
	"strings"

	"golang.org/x/sys/windows"
)

// checkOwnerOnly asserts, via the descriptor's SDDL, that the DACL is protected
// (P) and carries exactly one allow ACE, naming the current user.
func checkOwnerOnly(path string) error {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("read security info: %w", err)
	}
	sddl := sd.String()
	dacl := sddl[strings.Index(sddl, "D:"):]
	if !strings.HasPrefix(dacl, "D:P") {
		return fmt.Errorf("DACL is not protected, inherited entries still apply: %s", sddl)
	}
	if n := strings.Count(dacl, "(A;"); n != 1 {
		return fmt.Errorf("DACL has %d allow ACEs, want exactly 1: %s", n, sddl)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	if !strings.Contains(dacl, user.User.Sid.String()) {
		return fmt.Errorf("DACL does not name the current user: %s", sddl)
	}
	return nil
}
