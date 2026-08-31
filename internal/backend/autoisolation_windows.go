package backend

import (
	"strings"

	"golang.org/x/sys/windows/registry"
)

// autoIsolation picks the isolation mode for the local Windows edition. Process
// isolation needs an exact host/image build match and is only generally usable
// on Windows Server; client editions (Pro/Enterprise/IoT) must use Hyper-V.
func autoIsolation() string {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows NT\CurrentVersion`, registry.QUERY_VALUE)
	if err == nil {
		defer k.Close()
		if t, _, err := k.GetStringValue("InstallationType"); err == nil && !strings.HasPrefix(strings.ToLower(t), "server") {
			return "hyperv"
		}
	}
	return "process"
}
