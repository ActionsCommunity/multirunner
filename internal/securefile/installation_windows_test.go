//go:build windows

package securefile

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const testOperatorSID = "S-1-5-21-1-2-3-1001"
const testOtherSID = "S-1-5-21-1-2-3-1002"

func isolateInstallationOwner(t *testing.T, directory string) {
	t.Helper()
	canonical, err := canonicalInstallationDirectory(directory)
	if err != nil {
		t.Fatal(err)
	}
	installationOwners.Lock()
	previous, exists := installationOwners.owners[canonical]
	delete(installationOwners.owners, canonical)
	installationOwners.Unlock()
	t.Cleanup(func() {
		installationOwners.Lock()
		defer installationOwners.Unlock()
		if exists {
			installationOwners.owners[canonical] = previous
		} else {
			delete(installationOwners.owners, canonical)
		}
	})
}

type fakeBindingRegistryKey struct {
	descriptor string
	value      string
	kind       uint32
	hasValue   bool
	link       bool
}

// All binding tests use this in-memory registry; none reads or writes HKLM.
type fakeBindingRegistry struct {
	keys      map[registry.Key]*fakeBindingRegistryKey
	paths     map[string]registry.Key
	opened    []string
	writes    int
	beforeSet func()
	setErr    error
}

func newFakeBindingRegistry() *fakeBindingRegistry {
	fake := &fakeBindingRegistry{
		keys:  make(map[registry.Key]*fakeBindingRegistryKey),
		paths: make(map[string]registry.Key),
	}
	fake.add(registry.LOCAL_MACHINE, "")
	fake.add(1, "SOFTWARE")
	return fake
}

func (f *fakeBindingRegistry) add(parent registry.Key, name string) registry.Key {
	key := registry.Key(len(f.keys) + 1)
	f.paths[f.path(parent, name)] = key
	f.keys[key] = &fakeBindingRegistryKey{descriptor: bindingKeySDDL, kind: registry.SZ}
	return key
}

func (f *fakeBindingRegistry) path(parent registry.Key, name string) string {
	return string(rune(parent)) + `\` + name
}

func (f *fakeBindingRegistry) open(parent registry.Key, name string, create bool) (registry.Key, bool, error) {
	f.opened = append(f.opened, name)
	if key, exists := f.paths[f.path(parent, name)]; exists {
		return key, false, nil
	}
	if !create {
		return 0, false, windows.ERROR_FILE_NOT_FOUND
	}
	return f.add(parent, name), true, nil
}

func (f *fakeBindingRegistry) descriptor(key registry.Key) (*windows.SECURITY_DESCRIPTOR, error) {
	return windows.SecurityDescriptorFromString(f.keys[key].descriptor)
}

func (f *fakeBindingRegistry) value(key registry.Key, name string) (string, uint32, error) {
	node := f.keys[key]
	if name == "SymbolicLinkValue" {
		if node.link {
			return "redirect", registry.LINK, nil
		}
		return "", 0, windows.ERROR_FILE_NOT_FOUND
	}
	if !node.hasValue {
		return "", 0, windows.ERROR_FILE_NOT_FOUND
	}
	return node.value, node.kind, nil
}

func (f *fakeBindingRegistry) set(key registry.Key, value string) error {
	if f.beforeSet != nil {
		f.beforeSet()
	}
	if f.setErr != nil {
		return f.setErr
	}
	f.keys[key].value = value
	f.keys[key].hasValue = true
	f.writes++
	return nil
}

func (*fakeBindingRegistry) close(registry.Key) {}

func TestRegistryBindingDescriptorTrust(t *testing.T) {
	for _, test := range []struct {
		name, sddl string
		leaf, ok   bool
	}{
		{"protected binding", bindingKeySDDL, true, true},
		{"system owner", "O:SYD:P(A;;KA;;;SY)(A;;KA;;;BA)", true, true},
		{"operator owner", "O:" + testOperatorSID + "D:P(A;;KA;;;SY)(A;;KA;;;BA)", true, false},
		{"unprotected leaf", "O:BAD:(A;;KA;;;SY)(A;;KA;;;BA)", true, false},
		{"operator writable leaf", "O:BAD:P(A;;KA;;;SY)(A;;KA;;;" + testOperatorSID + ")", true, false},
		{"duplicate leaf", "O:BAD:P(A;;KA;;;SY)(A;;KA;;;SY)", true, false},
		{"inheritable leaf", "O:BAD:P(A;CI;KA;;;SY)(A;;KA;;;BA)", true, false},
		{"read only ancestor users", "O:BAD:(A;;KA;;;SY)(A;;KA;;;BA)(A;;KR;;;BU)", false, true},
		{"creator inherit only", "O:BAD:(A;;KA;;;SY)(A;CIIO;KA;;;CO)", false, true},
		{"creator effective write", "O:BAD:(A;;KA;;;SY)(A;;KA;;;CO)", false, false},
		{"untrusted ancestor owner", "O:" + testOperatorSID + "D:(A;;KA;;;SY)(A;;KA;;;BA)", false, false},
		{"ancestor delete", "O:BAD:(A;;SD;;;BU)", false, false},
		{"ancestor create subkey", "O:BAD:(A;;0x4;;;BU)", false, false},
		{"ancestor create link", "O:BAD:(A;;0x20;;;BU)", false, false},
		{"ancestor set value", "O:BAD:(A;;DC;;;BU)", false, false},
		{"ancestor change DACL", "O:BAD:(A;;WD;;;BU)", false, false},
		{"ancestor change owner", "O:BAD:(A;;WO;;;BU)", false, false},
		{"ancestor generic write", "O:BAD:(A;;GW;;;BU)", false, false},
		{"ancestor generic all", "O:BAD:(A;;GA;;;BU)", false, false},
		{"ancestor null DACL", "O:BAD:NO_ACCESS_CONTROL", false, false},
		{"trustedinstaller ancestor", "O:" + trustedInstallerSID + "D:(A;;KA;;;" + trustedInstallerSID + ")(A;;KR;;;BU)", false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			sd, err := windows.SecurityDescriptorFromString(test.sddl)
			if err != nil {
				t.Fatal(err)
			}
			err = validateRegistryBindingDescriptor(sd, test.leaf)
			if (err == nil) != test.ok {
				t.Fatalf("descriptor validation = %v, want success %v", err, test.ok)
			}
			if err != nil && !errors.Is(err, ErrInsecurePermissions) {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestOnlyActualElevatedOperatorCanProvisionBinding(t *testing.T) {
	for _, test := range []struct {
		name, current, marker string
		elevated, want        bool
	}{
		{"elevated original operator", testOperatorSID, testOperatorSID, true, true},
		{"unelevated original operator", testOperatorSID, testOperatorSID, false, false},
		{"another elevated operator", testOtherSID, testOperatorSID, true, false},
		{"LocalSystem cannot authenticate marker", "S-1-5-18", testOperatorSID, true, false},
		{"LocalSystem cannot bind itself", "S-1-5-18", "S-1-5-18", true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			current, err := windows.StringToSid(test.current)
			if err != nil {
				t.Fatal(err)
			}
			if got := canProvisionInstallationOwner(current, test.marker, test.elevated); got != test.want {
				t.Fatalf("provision allowed = %v, want %v", got, test.want)
			}
		})
	}
}

func TestRegistryBindingImmutableAndFailClosed(t *testing.T) {
	directory := `c:\operators\alice\multirunner`
	fake := newFakeBindingRegistry()
	store := registryBindingStore{io: fake}
	if err := provisionInstallationOwner(directory, testOperatorSID, store); err != nil {
		t.Fatal(err)
	}
	if err := provisionInstallationOwner(directory, testOperatorSID, store); err != nil {
		t.Fatal(err)
	}
	if err := provisionInstallationOwner(directory, testOtherSID, store); !errors.Is(err, ErrInsecurePermissions) {
		t.Fatalf("replacement binding error = %v", err)
	}
	if fake.writes != 1 {
		t.Fatalf("binding was overwritten: writes = %d", fake.writes)
	}
	value, err := store.read(directory)
	if err != nil || value != directory+"\n"+testOperatorSID {
		t.Fatalf("read binding = %q, %v", value, err)
	}
	if registryBindingAccess&registry.WOW64_64KEY == 0 ||
		registryBindingAccess&registry.WOW64_32KEY != 0 || registryOpenLink != 8 {
		t.Fatal("registry binding must use the explicit native 64-bit view and open links, not follow them")
	}
	for _, test := range []struct {
		name   string
		mutate func(*fakeBindingRegistry)
	}{
		{"writable ancestor", func(f *fakeBindingRegistry) {
			f.keys[2].descriptor = "O:BAD:(A;;KA;;;SY)(A;;KA;;;BA)(A;;KW;;;BU)"
		}},
		{"injected owner", func(f *fakeBindingRegistry) {
			f.keys[3].descriptor = "O:" + testOperatorSID + "D:P(A;;KA;;;SY)(A;;KA;;;BA)"
		}},
		{"registry link", func(f *fakeBindingRegistry) { f.keys[3].link = true }},
		{"unprotected leaf", func(f *fakeBindingRegistry) {
			f.keys[5].descriptor = "O:BAD:(A;;KA;;;SY)(A;;KA;;;BA)"
		}},
		{"expand string", func(f *fakeBindingRegistry) { f.keys[5].kind = registry.EXPAND_SZ }},
		{"incomplete create", func(f *fakeBindingRegistry) { f.keys[5].hasValue = false }},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newFakeBindingRegistry()
			s := registryBindingStore{io: f}
			if err := provisionInstallationOwner(directory, testOperatorSID, s); err != nil {
				t.Fatal(err)
			}
			test.mutate(f)
			if _, err := s.read(directory); err == nil {
				t.Fatal("unsafe binding read succeeded")
			}
			if err := provisionInstallationOwner(directory, testOperatorSID, s); err == nil {
				t.Fatal("unsafe binding was repaired or replaced")
			}
			if f.writes != 1 {
				t.Fatalf("existing binding was rewritten: %d", f.writes)
			}
		})
	}
}

func TestRegistryBindingConcurrentCreationAndInterruptedWrite(t *testing.T) {
	directory := `c:\operator\multirunner`
	fake := newFakeBindingRegistry()
	store := registryBindingStore{io: fake}
	fake.beforeSet = func() {
		if err := provisionInstallationOwner(directory, testOperatorSID, store); !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
			t.Fatalf("concurrent read of incomplete key = %v", err)
		}
	}
	if err := provisionInstallationOwner(directory, testOperatorSID, store); err != nil {
		t.Fatal(err)
	}
	if fake.writes != 1 {
		t.Fatalf("concurrent writes = %d", fake.writes)
	}
	fake = newFakeBindingRegistry()
	fake.setErr = windows.ERROR_ACCESS_DENIED
	store = registryBindingStore{io: fake}
	if err := provisionInstallationOwner(directory, testOperatorSID, store); !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		t.Fatalf("write error = %v", err)
	}
	fake.setErr = nil
	if err := provisionInstallationOwner(directory, testOperatorSID, store); !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		t.Fatalf("retry changed incomplete binding: %v", err)
	}
	if fake.writes != 0 {
		t.Fatal("incomplete binding was repaired")
	}
}

func TestInstallationBindingScopeAndCanonicalization(t *testing.T) {
	fake := newFakeBindingRegistry()
	store := registryBindingStore{io: fake}
	directory := `C:\Operators\Alice\multirunner`
	isolateInstallationOwner(t, directory)
	if err := provisionInstallationOwner(directory, testOperatorSID, store); err != nil {
		t.Fatal(err)
	}
	bound, err := bootstrapInstallationOwner(`c:\operators\alice\.\multirunner`, store)
	if err != nil || bound != testOperatorSID {
		t.Fatalf("bootstrap = %q, %v", bound, err)
	}
	system, _ := windows.StringToSid("S-1-5-18")
	admins, _ := windows.StringToSid("S-1-5-32-544")
	operator, _ := windows.StringToSid(testOperatorSID)
	for _, test := range []struct {
		path string
		want *windows.SID
	}{
		{directory + `\console.operator`, operator},
		{directory + `\console.secret`, operator},
		{directory + `\console.pairing`, operator},
		{directory + `\nested\console.secret`, admins},
		{directory + `-other\console.secret`, admins},
		{`C:\Operators\Alice\console.secret`, admins},
		{`C:\Other\multirunner\console.secret`, admins},
	} {
		if got := installationOwnerForPath(test.path, system, admins); !got.Equals(test.want) {
			t.Fatalf("owner for %q = %s, want %s", test.path, got.String(), test.want.String())
		}
	}
	if got := installationOwnerForPath(directory+`\console.secret`, operator, admins); !got.Equals(admins) {
		t.Fatal("foreground owner policy changed")
	}
	if _, err := bootstrapInstallationOwner(`c:\other\multirunner`, store); err == nil {
		t.Fatal("binding reused for a different directory")
	}
	for _, path := range []string{`\\server\share\dir`, `\\?\C:\dir`, `C:\dir.`, `C:\dir `, `C:\dir:stream`, "C:\\dir\ninjected"} {
		if _, err := canonicalInstallationDirectory(path); err == nil {
			t.Errorf("ambiguous path %q accepted", path)
		}
	}
	// Even a correctly protected key must bind the exact directory, not just
	// contain a parseable account SID or a copied binding from another install.
	for _, bad := range []string{
		`c:\other` + "\n" + testOperatorSID,
		strings.ToLower(directory) + "\nnot-a-sid",
		strings.ToLower(directory) + "\nS-1-5-18",
		strings.ToLower(directory) + "\n" + testOtherSID + "\nextra",
	} {
		fake.keys[5].value = bad
		if _, err := bootstrapInstallationOwner(directory, store); !errors.Is(err, ErrInsecurePermissions) {
			t.Fatalf("bad binding %q: %v", bad, err)
		}
	}
	fake.keys[5].value = strings.ToLower(directory) + "\n" + testOtherSID
	if _, err := bootstrapInstallationOwner(directory, store); !errors.Is(err, ErrInsecurePermissions) {
		t.Fatalf("in-process rebinding = %v", err)
	}
}

func TestConfiguredCredentialRequiresValidatedIndependentSameOperatorBinding(t *testing.T) {
	const path = `C:\explicit-oauth\github-user-token.json`
	isolateInstallationOwner(t, filepath.Dir(path))
	fake := newFakeBindingRegistry()
	store := registryBindingStore{io: fake}
	if err := provisionCredentialOwner(path, testOperatorSID, store, func(string) error {
		return ErrInsecurePermissions
	}); !errors.Is(err, ErrInsecurePermissions) {
		t.Fatalf("untrusted token/parent error = %v", err)
	}
	if fake.writes != 0 || len(fake.keys) != 2 {
		t.Fatal("token directory was bound before validating the token and parent chain")
	}
	checked := false
	if err := provisionCredentialOwner(path, testOperatorSID, store, func(actual string) error {
		if actual != path || fake.writes != 0 {
			t.Fatal("wrong token path or binding preceded secure token validation")
		}
		checked = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !checked || fake.writes != 1 {
		t.Fatal("validated configured credential directory was not independently bound")
	}
	checked = false
	if err := bootstrapCredentialOwner(path, testOtherSID, store, func(string) error {
		checked = true
		return nil
	}); !errors.Is(err, ErrInsecurePermissions) {
		t.Fatalf("different installation operator error = %v", err)
	}
	system, _ := windows.StringToSid("S-1-5-18")
	admins, _ := windows.StringToSid("S-1-5-32-544")
	if got := installationOwnerForPath(path, system, admins); !got.Equals(admins) || checked {
		t.Fatal("mismatched credential binding activated trust or reached a token read")
	}
	if err := bootstrapCredentialOwner(path, testOperatorSID, store, func(actual string) error {
		checked = actual == path
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !checked {
		t.Fatal("configured token was not checked after independent bootstrap")
	}
	operator, _ := windows.StringToSid(testOperatorSID)
	if got := installationOwnerForPath(path, system, admins); !got.Equals(operator) {
		t.Fatal("matching configured directory owner was not bootstrapped")
	}
	for _, unbound := range []string{`C:\explicit-oauth-other\token.json`, `C:\explicit-oauth\child\token.json`, `C:\token.json`} {
		if got := installationOwnerForPath(unbound, system, admins); !got.Equals(admins) {
			t.Fatalf("external credential binding widened trust to %q", unbound)
		}
	}
}

func TestLocalSystemReadsOperatorInstallationAfterIndependentBootstrap(t *testing.T) {
	directory, err := os.MkdirTemp(".", "installation-owner-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	isolateInstallationOwner(t, directory)
	system, _, current, admins, err := expectedSIDs()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "console.secret")
	if err := CreateExclusive(path, []byte("original service secret")); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(directory, "console.operator")
	if err := CreateExclusive(marker, []byte(current.String()+"\n")); err != nil {
		t.Fatal(err)
	}
	tokenPath := filepath.Join(directory, "github-user-token.json")
	if err := CreateExclusive(tokenPath, []byte("installation-scoped test token")); err != nil {
		t.Fatal(err)
	}
	outsideDirectory := filepath.Join(directory, "separate-auth-directory")
	isolateInstallationOwner(t, outsideDirectory)
	if err := os.Mkdir(outsideDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	outsideTokenPath := filepath.Join(outsideDirectory, "github-user-token.json")
	if err := CreateExclusive(outsideTokenPath, []byte("separately scoped test token")); err != nil {
		t.Fatal(err)
	}
	original := expectedSIDs
	expectedSIDs = func() (*windows.SID, *windows.SID, *windows.SID, *windows.SID, error) {
		return system, admins, system, admins, nil
	}
	t.Cleanup(func() { expectedSIDs = original })
	if _, err := Read(marker); !errors.Is(err, ErrInsecurePermissions) {
		t.Fatalf("unbound LocalSystem marker read = %v", err)
	}
	if _, err := Read(path); !errors.Is(err, ErrInsecurePermissions) {
		t.Fatalf("unbound LocalSystem secret read = %v", err)
	}
	fake := newFakeBindingRegistry()
	store := registryBindingStore{io: fake}
	if _, err := bootstrapInstallationOwner(directory, store); err == nil {
		t.Fatal("LocalSystem trusted marker contents without an independent binding")
	}
	if err := provisionInstallationOwner(directory, current.String(), store); err != nil {
		t.Fatal(err)
	}
	if _, err := bootstrapInstallationOwner(directory, store); err != nil {
		t.Fatal(err)
	}
	if data, err := Read(marker); err != nil || strings.TrimSpace(string(data)) != current.String() {
		t.Fatalf("bound operator marker read = %q, %v", data, err)
	}
	if data, err := Read(path); err != nil || string(data) != "original service secret" {
		t.Fatalf("bound service secret read = %q, %v", data, err)
	}
	if data, err := Read(tokenPath); err != nil || string(data) != "installation-scoped test token" {
		t.Fatalf("bound GitHub token read = %q, %v", data, err)
	}
	if _, err := Read(outsideTokenPath); !errors.Is(err, ErrInsecurePermissions) {
		t.Fatalf("unbound external GitHub token read = %v", err)
	}
	// Service install/start validates this explicitly configured token as the
	// original operator, then records a separate binding. No marker is created
	// in the credential directory and no trust is inherited from the DB scope.
	syntheticSystem := expectedSIDs
	expectedSIDs = original
	if err := provisionCredentialOwner(outsideTokenPath, current.String(), store, validateCredentialFile); err != nil {
		t.Fatal(err)
	}
	expectedSIDs = syntheticSystem
	if err := bootstrapCredentialOwner(outsideTokenPath, current.String(), store, validateCredentialFile); err != nil {
		t.Fatal(err)
	}
	if data, err := Read(outsideTokenPath); err != nil || string(data) != "separately scoped test token" {
		t.Fatalf("independently bound external GitHub token read = %q, %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(outsideDirectory, "console.operator")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("credential directory unexpectedly required a console operator marker: %v", err)
	}
	// A protected DACL must still pass the original strict identity rules.
	sd, err := windows.SecurityDescriptorFromString("D:P(A;;GA;;;SY)(A;;GA;;;BU)")
	if err != nil {
		t.Fatal(err)
	}
	acl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, acl, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(path); !errors.Is(err, ErrInsecurePermissions) {
		t.Fatalf("bound service accepted an unrelated account DACL: %v", err)
	}
}
