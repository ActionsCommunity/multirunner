package ghapp

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

var systemTokenPath = flag.String("system-token", "", "credential path for the LocalSystem test worker")

// Exercise actual file reads and replacements as the default SCM identity.
// A scheduled task gives the worker LocalSystem's token without installing a
// persistent service. CI's Windows administrator account can create this task.
func TestCredentialsWorkAcrossLocalSystemRefresh(t *testing.T) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		t.Fatal(err)
	}
	if user.User.Sid.Equals(system) {
		t.Skip("requires an interactive account distinct from LocalSystem")
	}
	// Keep /TR below schtasks' command-length limit, including long Go build
	// paths. t.TempDir embeds the full test name in the directory name.
	dir, err := os.MkdirTemp("", "mr-system-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Error(err)
		}
	})
	path := filepath.Join(dir, "token.json")
	if err := SaveUserToken(path, &UserToken{AccessToken: "operator-token"}); err != nil {
		t.Fatal(err)
	}
	if err := WriteSecretFile(path+".key", []byte("private-key")); err != nil {
		t.Fatal(err)
	}
	// A two-entry ACL still must not allow a broad group to be mistaken for
	// the operator when the service preserves access on refresh.
	public := path + ".public"
	if err := os.WriteFile(public, []byte("test-only"), 0600); err != nil {
		t.Fatal(err)
	}
	everyone, err := windows.CreateWellKnownSid(windows.WinWorldSid)
	if err != nil {
		t.Fatal(err)
	}
	entries := []windows.EXPLICIT_ACCESS{}
	for _, sid := range []*windows.SID{system, everyone} {
		entries = append(entries, windows.EXPLICIT_ACCESS{AccessPermissions: windows.GENERIC_ALL, AccessMode: windows.GRANT_ACCESS,
			Trustee: windows.TRUSTEE{TrusteeForm: windows.TRUSTEE_IS_SID, TrusteeValue: windows.TrusteeValueFromSID(sid)}})
	}
	acl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(public, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("multirunner-credential-test-%d-%d", os.Getpid(), time.Now().UnixNano())
	command := fmt.Sprintf(`"%s" -test.run=^TestSystemCredentialWorker$ -system-token="%s"`, exe, path)
	output, err := exec.Command("schtasks.exe", "/Create", "/TN", name, "/TR", command, "/SC", "ONCE", "/ST", "23:59", "/RU", "SYSTEM", "/RL", "HIGHEST", "/F").CombinedOutput()
	if err != nil {
		// Ordinary user machines may not grant task administration. The ACL
		// tests still run there; the CI administrator must exercise the worker.
		if os.Getenv("CI") == "" {
			t.Skipf("LocalSystem task unavailable: %v: %s", err, output)
		}
		t.Fatalf("create LocalSystem task: %v: %s", err, output)
	}
	t.Cleanup(func() { _ = exec.Command("schtasks.exe", "/Delete", "/TN", name, "/F").Run() })
	if output, err := exec.Command("schtasks.exe", "/Run", "/TN", name).CombinedOutput(); err != nil {
		t.Fatalf("run task: %v: %s", err, output)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		data, err := os.ReadFile(path + ".result")
		if err == nil {
			if string(data) != "ok" {
				t.Fatalf("LocalSystem worker: %s", data)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("LocalSystem worker did not finish: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	tok, err := LoadUserToken(path)
	if err != nil || tok.AccessToken != "system-rotation-2" {
		t.Fatalf("operator lost access after rotation: %+v, %v", tok, err)
	}
	if err := CheckOwnerOnly(path); err != nil {
		t.Fatal(err)
	}
}

func TestSystemCredentialWorker(t *testing.T) {
	if *systemTokenPath == "" {
		t.Skip("only run by the LocalSystem task")
	}
	path := *systemTokenPath
	result := "ok"
	defer func() {
		if err := os.WriteFile(path+".result", []byte(result), 0600); err != nil {
			t.Error(err)
		}
	}()
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		result = err.Error()
		t.Fatal(err)
	}
	if !user.User.Sid.IsWellKnown(windows.WinLocalSystemSid) {
		result = "worker is not LocalSystem"
		t.Fatal(result)
	}
	if err := CheckOwnerOnly(path + ".public"); err == nil {
		result = "LocalSystem accepted an Everyone ACE as operator access"
		t.Fatal(result)
	}
	data, err := os.ReadFile(path + ".key")
	if err != nil || strings.TrimSpace(string(data)) != "private-key" {
		result = fmt.Sprintf("read key: %q, %v", data, err)
		t.Fatal(result)
	}
	for i := 1; i <= 2; i++ {
		if _, err := LoadUserToken(path); err != nil {
			result = "read token: " + err.Error()
			t.Fatal(result)
		}
		if err := SaveUserToken(path, &UserToken{AccessToken: fmt.Sprintf("system-rotation-%d", i)}); err != nil {
			result = "save token: " + err.Error()
			t.Fatal(result)
		}
	}
}
