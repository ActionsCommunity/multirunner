package winsetup

import "golang.org/x/sys/windows/registry"

var openRegistryKey = registry.OpenKey

// rebootPending checks the Windows servicing marker used when optional feature
// changes, including Containers and Hyper-V, require a reboot.
func rebootPending() bool {
	if k, err := openRegistryKey(registry.LOCAL_MACHINE,
		`SOFTWARE\Microsoft\Windows\CurrentVersion\Component Based Servicing\RebootPending`,
		registry.READ); err == nil {
		k.Close()
		return true
	}
	return false
}
