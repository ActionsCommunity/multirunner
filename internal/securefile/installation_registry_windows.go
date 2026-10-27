//go:build windows

package securefile

import (
	"errors"
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const bindingKeySDDL = "O:BAG:BAD:P(A;;KA;;;SY)(A;;KA;;;BA)"
const registryOpenLink = 0x00000008
const registryBindingAccess = registry.READ | registry.WOW64_64KEY

var regCreateKeyEx = windows.NewLazySystemDLL("advapi32.dll").NewProc("RegCreateKeyExW")

type bindingRegistry interface {
	open(parent registry.Key, name string, create bool) (registry.Key, bool, error)
	descriptor(registry.Key) (*windows.SECURITY_DESCRIPTOR, error)
	value(registry.Key, string) (string, uint32, error)
	set(registry.Key, string) error
	close(registry.Key)
}

type registryBindingStore struct {
	io bindingRegistry
}

func (s registryBindingStore) read(directory string) (string, error) {
	return s.binding(directory, nil)
}

func (s registryBindingStore) create(directory, value string) (string, error) {
	return s.binding(directory, &value)
}

func (s registryBindingStore) binding(directory string, proposed *string) (string, error) {
	io := s.io
	if io == nil {
		io = windowsBindingRegistry{}
	}
	parent := registry.LOCAL_MACHINE
	parts := []string{"", "SOFTWARE", "Multirunner", "ConsoleInstallations", installationBindingName(directory)}
	for index, part := range parts {
		key, created, err := io.open(parent, part, proposed != nil && index >= 2)
		if err != nil {
			return "", err
		}
		defer io.close(key)
		// Opening one component at a time with OPEN_LINK prevents an attacker
		// from redirecting an otherwise trusted registry path through a link.
		if _, _, err := io.value(key, "SymbolicLinkValue"); !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
			return "", fmt.Errorf("%w: registry binding ancestry contains a link or unreadable key", ErrInsecurePermissions)
		}
		sd, err := io.descriptor(key)
		if err != nil {
			return "", err
		}
		leaf := index == len(parts)-1
		if err := validateRegistryBindingDescriptor(sd, leaf); err != nil {
			return "", err
		}
		if !leaf {
			parent = key
			continue
		}
		if created {
			// Only the creator of the key writes. A concurrent installer sees
			// either this complete value or a missing value and fails closed;
			// neither it nor a later installer can replace an existing binding.
			if proposed == nil {
				return "", fmt.Errorf("%w: unexpected registry key creation", ErrInsecurePermissions)
			}
			if err := io.set(key, *proposed); err != nil {
				return "", err
			}
		}
		value, kind, err := io.value(key, "")
		if err != nil {
			if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
				return "", fmt.Errorf("installation owner key is incomplete; administrator repair is required (existing bindings are never overwritten): %w", err)
			}
			return "", err
		}
		if kind != registry.SZ {
			return "", fmt.Errorf("%w: installation binding is not a string", ErrInsecurePermissions)
		}
		return value, nil
	}
	panic("unreachable registry binding path")
}

func validateRegistryBindingDescriptor(sd *windows.SECURITY_DESCRIPTOR, leaf bool) error {
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return err
	}
	administrators, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return err
	}
	installer, err := windows.StringToSid(trustedInstallerSID)
	if err != nil {
		return err
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return err
	}
	if owner == nil || (!owner.Equals(system) && !owner.Equals(administrators) &&
		(leaf || !owner.Equals(installer))) {
		return fmt.Errorf("%w: registry binding ancestry has an untrusted owner", ErrInsecurePermissions)
	}
	control, _, err := sd.Control()
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	if dacl == nil || (leaf && (control&windows.SE_DACL_PROTECTED == 0 || dacl.AceCount != 2)) {
		return fmt.Errorf("%w: registry binding DACL is not protected", ErrInsecurePermissions)
	}
	const writable = windows.WRITE_DAC | windows.WRITE_OWNER | windows.DELETE |
		windows.GENERIC_WRITE | windows.GENERIC_ALL | registry.SET_VALUE |
		registry.CREATE_SUB_KEY | registry.CREATE_LINK
	var systemFound, administratorsFound bool
	for index := uint32(0); index < uint32(dacl.AceCount); index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, index, &ace); err != nil {
			return err
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE &&
			ace.Header.AceType != windows.ACCESS_DENIED_ACE_TYPE {
			return fmt.Errorf("%w: unsupported registry access entry", ErrInsecurePermissions)
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if leaf {
			if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE ||
				ace.Header.AceFlags != 0 || ace.Mask != registry.ALL_ACCESS {
				return fmt.Errorf("%w: unexpected registry binding access entry", ErrInsecurePermissions)
			}
			switch {
			case sid.Equals(system) && !systemFound:
				systemFound = true
			case sid.Equals(administrators) && !administratorsFound:
				administratorsFound = true
			default:
				return fmt.Errorf("%w: unexpected registry binding account", ErrInsecurePermissions)
			}
		} else if ace.Header.AceType == windows.ACCESS_ALLOWED_ACE_TYPE &&
			ace.Header.AceFlags&windows.INHERIT_ONLY_ACE == 0 && ace.Mask&writable != 0 &&
			!sid.Equals(system) && !sid.Equals(administrators) && !sid.Equals(installer) {
			return fmt.Errorf("%w: registry binding ancestor is writable by an untrusted account", ErrInsecurePermissions)
		}
	}
	if leaf && (!systemFound || !administratorsFound) {
		return fmt.Errorf("%w: registry binding accounts are missing", ErrInsecurePermissions)
	}
	runtime.KeepAlive(sd)
	return nil
}

type windowsBindingRegistry struct{}

func (windowsBindingRegistry) open(parent registry.Key, name string, create bool) (registry.Key, bool, error) {
	subkey, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return 0, false, err
	}
	var handle windows.Handle
	if name != "" {
		err = windows.RegOpenKeyEx(windows.Handle(parent), subkey, registryOpenLink, registryBindingAccess, &handle)
		if !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || !create {
			return registry.Key(handle), false, err
		}
	}
	// RegOpenKeyEx with an empty subkey returns the predefined pseudo-handle.
	// RegCreateKeyEx with an empty subkey instead reopens that existing root
	// with a real READ_CONTROL handle; it cannot create or modify the root.
	access := registryBindingAccess
	if name != "" {
		access |= registry.SET_VALUE
	}
	sd, err := windows.SecurityDescriptorFromString(bindingKeySDDL)
	if err != nil {
		return 0, false, err
	}
	attributes := windows.SecurityAttributes{
		Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd,
	}
	var disposition uint32
	result, _, _ := regCreateKeyEx.Call(uintptr(parent), uintptr(unsafe.Pointer(subkey)),
		0, 0, 0, uintptr(access),
		uintptr(unsafe.Pointer(&attributes)), uintptr(unsafe.Pointer(&handle)), uintptr(unsafe.Pointer(&disposition)))
	runtime.KeepAlive(sd)
	runtime.KeepAlive(subkey)
	if result != 0 {
		return 0, false, windows.Errno(result)
	}
	return registry.Key(handle), disposition == 1, nil
}

func (windowsBindingRegistry) descriptor(key registry.Key) (*windows.SECURITY_DESCRIPTOR, error) {
	return windows.GetSecurityInfo(windows.Handle(key), windows.SE_REGISTRY_KEY,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
}

func (windowsBindingRegistry) value(key registry.Key, name string) (string, uint32, error) {
	buffer := make([]uint16, 32768)
	bytes := unsafe.Slice((*byte)(unsafe.Pointer(&buffer[0])), len(buffer)*2)
	n, kind, err := key.GetValue(name, bytes)
	if err != nil {
		return "", kind, err
	}
	if kind != registry.SZ {
		return "", kind, nil
	}
	if n < 2 || n%2 != 0 || buffer[n/2-1] != 0 {
		return "", kind, fmt.Errorf("%w: malformed registry binding string", ErrInsecurePermissions)
	}
	for _, char := range buffer[:n/2-1] {
		if char == 0 {
			return "", kind, fmt.Errorf("%w: registry binding contains an embedded NUL", ErrInsecurePermissions)
		}
	}
	return windows.UTF16ToString(buffer[:n/2]), kind, nil
}

func (windowsBindingRegistry) set(key registry.Key, value string) error {
	return key.SetStringValue("", value)
}

func (windowsBindingRegistry) close(key registry.Key) {
	_ = key.Close()
}
