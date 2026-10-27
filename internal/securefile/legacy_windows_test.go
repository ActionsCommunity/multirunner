//go:build windows

package securefile

import (
	"testing"

	"golang.org/x/sys/windows"
)

func TestLegacyDACLRepairableOnlyForInheritedIndividualAccess(t *testing.T) {
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		t.Fatal(err)
	}
	administrators, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		t.Fatal(err)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	current := user.User.Sid.String()
	cases := []struct {
		name string
		sddl string
		want bool
	}{
		{"inherited system admins user", "O:BAD:AI(A;ID;FA;;;SY)(A;ID;FA;;;BA)(A;ID;FA;;;" + current + ")", true},
		{"inherited system admins", "O:BAD:AI(A;ID;FA;;;SY)(A;ID;FA;;;BA)", true},
		{"already protected", "O:BAD:PAI(A;;FA;;;SY)(A;;FA;;;BA)", false},
		{"explicit entry", "O:BAD:AI(A;;FA;;;SY)(A;ID;FA;;;BA)", false},
		{"users group", "O:BAD:AI(A;ID;FA;;;SY)(A;ID;FA;;;BU)", false},
		{"authenticated users", "O:BAD:AI(A;ID;FA;;;SY)(A;ID;FR;;;AU)", false},
		{"everyone", "O:BAD:AI(A;ID;FA;;;SY)(A;ID;FR;;;WD)", false},
		{"deny entry", "O:BAD:AI(D;ID;FA;;;BG)(A;ID;FA;;;SY)", false},
		{"empty dacl", "O:BAD:AI", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sd, err := windows.SecurityDescriptorFromString(tc.sddl)
			if err != nil {
				t.Fatal(err)
			}
			if got := legacyDACLRepairable(sd, system, administrators); got != tc.want {
				t.Fatalf("legacyDACLRepairable(%s) = %v, want %v", tc.sddl, got, tc.want)
			}
		})
	}
}
