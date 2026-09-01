package imageversions

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEmbeddedManifest(t *testing.T) {
	m, err := Embedded()
	if err != nil {
		t.Fatal(err)
	}
	defaultNode, ok := m.Node.DefaultRelease()
	if !ok || defaultNode.Version == "" {
		t.Fatal("default Node release is unresolved")
	}
	if len(m.DotNet.ChannelsForTarget(DotNetTargetQEMUWindows)) == 0 {
		t.Fatal("no .NET channel targets QEMU")
	}
}

func TestReadForUpdateAllowsNewUnresolvedEntries(t *testing.T) {
	m := MustEmbedded()
	m.Node.Releases["26"] = NodeRelease{}
	m.DotNet.Channels["11.0"] = DotNetChannel{Targets: []string{DotNetTargetLinux}}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "versions.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadForUpdate(path); err != nil {
		t.Fatalf("policy-only read rejected new entries: %v", err)
	}
	if _, err := Read(path); err == nil {
		t.Fatal("strict read accepted unresolved entries")
	}
}

func TestDockerfileBaseDefaultsMatchManifest(t *testing.T) {
	m := MustEmbedded()
	for file, reference := range map[string]string{
		"linux/Dockerfile":   m.Minimal.LinuxBase.Reference(),
		"windows/Dockerfile": m.Minimal.WindowsBase.Reference(),
	} {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), "ARG BASE="+reference+"\n") {
			t.Errorf("%s base default does not match versions.json", file)
		}
	}
}
