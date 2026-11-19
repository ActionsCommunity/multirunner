//go:build windows

package secureartifact

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestPrepareRootWithModifyOnlyParent(t *testing.T) {
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
	if err := os.Mkdir(probe, 0o700); err != nil {
		t.Fatal(err)
	}
	name, err := windows.UTF16PtrFromString(probe)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateFile(name, windows.WRITE_OWNER,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
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
	root, err := PrepareRoot(filepath.Join(parent, "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	after, err := windows.GetNamedSecurityInfo(root, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	owner, _, err := after.Owner()
	if err != nil || owner == nil || !owner.Equals(creator) {
		t.Fatalf("creator owner was not preserved: %v", err)
	}
	path := filepath.Join(root, "artifact")
	if err := os.WriteFile(path, []byte("artifact"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := Open(root, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestArtifactSecurityDescriptorSyntheticOwners(t *testing.T) {
	current, err := windows.StringToSid("S-1-5-21-1-2-3-1001")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, owner string
		elevated    bool
		wantOK      bool
	}{
		{"creator owner", current.String(), false, true},
		{"elevated creator owner", current.String(), true, true},
		{"elevated administrators owner", "BA", true, true},
		{"non elevated administrators owner", "BA", false, false},
		{"unrelated owner", "S-1-5-21-1-2-3-1002", false, false},
		{"elevated unrelated owner", "S-1-5-21-1-2-3-1002", true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			sd, err := windows.SecurityDescriptorFromString(
				"O:" + test.owner + "D:P(A;OICI;GA;;;SY)(A;OICI;GA;;;" + current.String() + ")")
			if err != nil {
				t.Fatal(err)
			}
			err = validateSecurityDescriptor(sd, current, test.elevated)
			if test.wantOK && err != nil {
				t.Fatal(err)
			}
			if !test.wantOK && !errors.Is(err, ErrInsecure) {
				t.Fatalf("validation error = %v", err)
			}
		})
	}
}

func TestPrepareRootRejectsInheritedDirectorySecurity(t *testing.T) {
	root := filepath.Join(t.TempDir(), "existing")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}

	if _, err := PrepareRoot(root); !errors.Is(err, ErrInsecure) {
		t.Fatalf("inherited root security error = %v", err)
	}
}

func TestOpenAcceptsArtifactCreatedInProtectedRoot(t *testing.T) {
	root, err := PrepareRoot(filepath.Join(t.TempDir(), "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "artifact")
	if err := os.WriteFile(path, []byte("artifact"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := Open(root, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}
