//go:build windows

package securefile

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestCreateAndReplaceWithModifyOnlyParent(t *testing.T) {
	if windows.GetCurrentProcessToken().IsElevated() {
		t.Skip("requires a non-elevated token to verify WRITE_OWNER is denied")
	}
	parent, err := os.MkdirTemp(".", "modify-only-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(parent) })
	current, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		t.Fatal(err)
	}
	// Modify excludes WRITE_DAC and WRITE_OWNER. Child creators retain the
	// implicit owner right to change their DACL, not their owner.
	entries := []windows.EXPLICIT_ACCESS{
		{
			AccessPermissions: 0x1301bf,
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeValue: windows.TrusteeValueFromSID(current.User.Sid),
			},
		},
		{
			AccessPermissions: windows.GENERIC_ALL,
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeValue: windows.TrusteeValueFromSID(system),
			},
		},
	}
	acl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(parent, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, acl, nil); err != nil {
		t.Fatal(err)
	}
	probe := filepath.Join(parent, "probe")
	if err := os.WriteFile(probe, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	name, err := windows.UTF16PtrFromString(probe)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateFile(name, windows.WRITE_OWNER,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err == nil {
		windows.CloseHandle(handle)
		t.Fatal("fixture unexpectedly grants WRITE_OWNER; run with a non-elevated user token")
	}
	if !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		t.Fatalf("WRITE_OWNER probe: %v", err)
	}
	before, err := windows.GetNamedSecurityInfo(probe, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	creator, _, err := before.Owner()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(parent, "secret")
	if err := CreateExclusive(path, []byte("first")); err != nil {
		t.Fatal(err)
	}
	if err := Replace(path, []byte("replacement")); err != nil {
		t.Fatal(err)
	}
	data, err := Read(path)
	if err != nil || !bytes.Equal(data, []byte("replacement")) {
		t.Fatalf("Read = %q, %v", data, err)
	}
	after, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	owner, _, err := after.Owner()
	if err != nil || owner == nil || !owner.Equals(creator) {
		t.Fatalf("creator owner was not preserved: %v", err)
	}
}

func TestTrustedParentOwnerSyntheticIdentities(t *testing.T) {
	sid := func(value string) *windows.SID {
		t.Helper()
		result, err := windows.StringToSid(value)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	system := sid("S-1-5-18")
	administrators := sid("S-1-5-32-544")
	operator := sid("S-1-5-21-1-2-3-1001")
	unrelated := sid("S-1-5-21-1-2-3-1002")
	trustedInstaller := sid(trustedInstallerSID)
	for _, test := range []struct {
		name                         string
		owner, installation, current *windows.SID
		want                         bool
	}{
		{"system administrator ancestor", administrators, administrators, system, true},
		{"system unbound operator ancestor", operator, administrators, system, false},
		// An installation SID must come from an authenticated binding, not the
		// ancestor being examined.
		{"system explicitly bound operator ancestor", operator, operator, system, true},
		{"system unrelated ancestor", unrelated, operator, system, false},
		{"creator owned ancestor", operator, operator, operator, true},
		{"elevated creator owned ancestor", operator, administrators, operator, true},
		{"missing owner", nil, administrators, system, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := trustedParentOwner(test.owner, system, test.installation,
				test.current, administrators, trustedInstaller); got != test.want {
				t.Fatalf("trustedParentOwner = %v, want %v", got, test.want)
			}
		})
	}
}

func TestSecretSecurityDescriptorSyntheticIdentities(t *testing.T) {
	for _, test := range []struct {
		name, descriptor, current, installation string
		wantOK                                  bool
	}{
		{"creator owner", "O:S-1-5-21-1-2-3-1001D:P(A;;GA;;;SY)(A;;GA;;;S-1-5-21-1-2-3-1001)", "S-1-5-21-1-2-3-1001", "S-1-5-21-1-2-3-1001", true},
		{"elevated creator owner", "O:S-1-5-21-1-2-3-1001D:P(A;;GA;;;SY)(A;;GA;;;BA)", "S-1-5-21-1-2-3-1001", "S-1-5-32-544", true},
		{"elevated administrators owner", "O:BAD:P(A;;GA;;;SY)(A;;GA;;;BA)", "S-1-5-21-1-2-3-1001", "S-1-5-32-544", true},
		{"system administrators owner", "O:BAD:P(A;;GA;;;SY)(A;;GA;;;BA)", "S-1-5-18", "S-1-5-32-544", true},
		{"system unbound operator owner", "O:S-1-5-21-1-2-3-1001D:P(A;;GA;;;SY)(A;;GA;;;BA)", "S-1-5-18", "S-1-5-32-544", false},
		{"system independently bound operator owner", "O:S-1-5-21-1-2-3-1001D:P(A;;GA;;;SY)(A;;GA;;;BA)", "S-1-5-18", "S-1-5-21-1-2-3-1001", true},
		{"system bound owner operator DACL", "O:S-1-5-21-1-2-3-1001D:P(A;;GA;;;SY)(A;;GA;;;S-1-5-21-1-2-3-1001)", "S-1-5-18", "S-1-5-21-1-2-3-1001", true},
		{"system binding rejects unrelated owner", "O:S-1-5-21-1-2-3-1002D:P(A;;GA;;;SY)(A;;GA;;;BA)", "S-1-5-18", "S-1-5-21-1-2-3-1001", false},
		{"system binding rejects unrelated access", "O:S-1-5-21-1-2-3-1001D:P(A;;GA;;;SY)(A;;GA;;;S-1-5-21-1-2-3-1002)", "S-1-5-18", "S-1-5-21-1-2-3-1001", false},
		{"unrelated owner", "O:S-1-5-21-1-2-3-1002D:P(A;;GA;;;SY)(A;;GA;;;BA)", "S-1-5-21-1-2-3-1001", "S-1-5-32-544", false},
		{"unrelated access", "O:BAD:P(A;;GA;;;SY)(A;;GA;;;S-1-5-21-1-2-3-1002)", "S-1-5-18", "S-1-5-32-544", false},
		{"unprotected DACL", "O:BAD:(A;;GA;;;SY)(A;;GA;;;BA)", "S-1-5-18", "S-1-5-32-544", false},
		{"inheritable ACE", "O:BAD:P(A;OI;GA;;;SY)(A;;GA;;;BA)", "S-1-5-18", "S-1-5-32-544", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			sd, err := windows.SecurityDescriptorFromString(test.descriptor)
			if err != nil {
				t.Fatal(err)
			}
			parse := func(value string) *windows.SID {
				t.Helper()
				result, err := windows.StringToSid(value)
				if err != nil {
					t.Fatal(err)
				}
				return result
			}
			err = validateSecurityDescriptor(sd, parse("S-1-5-18"),
				parse(test.installation), parse(test.current), parse("S-1-5-32-544"))
			if test.wantOK && err != nil {
				t.Fatal(err)
			}
			if !test.wantOK && !errors.Is(err, ErrInsecurePermissions) {
				t.Fatalf("validation error = %v", err)
			}
		})
	}
}

func TestReadRejectsOrdinaryInheritedDACL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ordinary.secret")
	if err := os.WriteFile(path, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(path); !errors.Is(err, ErrInsecurePermissions) {
		t.Fatalf("ordinary file error = %v", err)
	}
}

func TestReadRejectsUnexpectedAccountDACL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unexpected-account.secret")
	if err := os.WriteFile(path, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		t.Fatal(err)
	}
	users, err := windows.CreateWellKnownSid(windows.WinBuiltinUsersSid)
	if err != nil {
		t.Fatal(err)
	}
	entries := []windows.EXPLICIT_ACCESS{
		{
			AccessPermissions: windows.GENERIC_ALL,
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       windows.NO_INHERITANCE,
			Trustee: windows.TRUSTEE{
				TrusteeForm: windows.TRUSTEE_IS_SID, TrusteeType: windows.TRUSTEE_IS_USER,
				TrusteeValue: windows.TrusteeValueFromSID(system),
			},
		},
		{
			AccessPermissions: windows.GENERIC_ALL,
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       windows.NO_INHERITANCE,
			Trustee: windows.TRUSTEE{
				TrusteeForm: windows.TRUSTEE_IS_SID, TrusteeType: windows.TRUSTEE_IS_GROUP,
				TrusteeValue: windows.TrusteeValueFromSID(users),
			},
		},
	}
	acl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(
		path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, acl, nil,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(path); !errors.Is(err, ErrInsecurePermissions) {
		t.Fatalf("unexpected-account DACL error = %v", err)
	}
}
