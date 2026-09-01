package winvm

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
)

func lookPathDir(bin string) (string, error) {
	p, err := exec.LookPath(bin)
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	return filepath.Dir(abs), nil
}

// Firmware holds UEFI (OVMF/edk2) firmware paths. CodeFD empty => legacy BIOS.
// UEFI is used for Windows because it skips the "Press any key to boot from CD"
// prompt that hangs unattended BIOS installs.
type Firmware struct {
	CodeFD string // read-only firmware code (edk2-x86_64-code.fd / OVMF_CODE.fd)
	VarsFD string // writable per-VM NVRAM vars copy
}

// LaunchOpts describes one ephemeral Windows VM launch.
type LaunchOpts struct {
	Golden     string // base (golden) qcow2, read-only backing file
	Overlay    string // per-job copy-on-write overlay qcow2
	JITISOPath string // config ISO carrying the JIT blob
	MemMB      int
	CPUs       int
	Accel      string // "" = tcg; callers normally resolve host-compatible auto mode
	Firmware   Firmware
}

// DetectOVMF locates UEFI firmware: the code blob and a vars template to copy.
// Searches the QEMU install's share dir and common Linux locations.
func DetectOVMF(qemuBin string) (codeFD, varsTemplate string) {
	var dirs []string
	if p, err := lookPathDir(qemuBin); err == nil {
		dirs = append(dirs, filepath.Join(p, "share"), filepath.Join(p, "..", "share", "qemu"))
	}
	dirs = append(dirs,
		"/usr/share/OVMF", "/usr/share/edk2/x64", "/usr/share/edk2-ovmf/x64",
		"/usr/share/qemu", "/usr/share/edk2/ovmf",
		"/usr/share/pve-edk2-firmware") // Proxmox
	codes := []string{"edk2-x86_64-code.fd", "OVMF_CODE.fd", "OVMF_CODE_4M.fd", "OVMF_CODE.secboot.fd"}
	varss := []string{"edk2-i386-vars.fd", "OVMF_VARS.fd", "OVMF_VARS_4M.fd"}
	for _, d := range dirs {
		c := firstExisting(d, codes)
		v := firstExisting(d, varss)
		if c != "" && v != "" {
			return c, v
		}
	}
	return "", ""
}

func firstExisting(dir string, names []string) string {
	for _, n := range names {
		p := filepath.Join(dir, n)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// DetectAccel picks the QEMU accelerator for this backend's x86-64 Windows
// guest. Hardware virtualization requires the host and guest CPU architectures
// to match, so ARM hosts use QEMU's cross-architecture TCG emulation.
func DetectAccel(goos, goarch string) string {
	if goarch != "amd64" {
		return "tcg"
	}
	switch goos {
	case "linux":
		return "kvm"
	case "windows":
		return "whpx"
	case "darwin":
		return "hvf"
	default:
		return "tcg"
	}
}

// OverlayCreateArgs builds the qemu-img args to create a clean copy-on-write
// overlay backed by the golden image (so each job starts from a pristine disk).
func OverlayCreateArgs(golden, overlay string) []string {
	return []string{
		"create", "-q",
		"-f", "qcow2",
		"-F", "qcow2",
		"-b", golden,
		overlay,
	}
}

// QEMUArgs builds the qemu-system-x86_64 args for a headless ephemeral runner VM.
// The guest auto-runs the runner and powers off after its one job; -no-reboot
// makes that power-off terminate QEMU so the backend's Wait returns.
func QEMUArgs(o LaunchOpts) []string {
	accel := o.Accel
	if accel == "" {
		accel = "tcg"
	}
	mem := o.MemMB
	if mem <= 0 {
		mem = 4096
	}
	cpus := o.CPUs
	if cpus <= 0 {
		cpus = 2
	}
	// AHCI disk + e1000 NIC: both have inbox Windows drivers (virtio would need
	// driver injection during install).
	args := []string{
		"-machine", "q35",
		"-accel", accel,
		// Must match the bake's CPU model (cpuArg) so the golden boots on the vCPU
		// it was installed on. WHPX gets Hyper-V enlightenments for stability.
		"-cpu", cpuArg(accel),
		"-m", strconv.Itoa(mem),
		"-smp", strconv.Itoa(cpus),
	}
	if o.Firmware.CodeFD != "" {
		// UEFI: skips the BIOS "Press any key to boot from CD" prompt.
		args = append(args,
			"-drive", "if=pflash,format=raw,unit=0,readonly=on,file="+o.Firmware.CodeFD,
			"-drive", "if=pflash,format=raw,unit=1,file="+o.Firmware.VarsFD)
	}
	args = append(args,
		"-drive", fmt.Sprintf("file=%s,if=none,id=osdisk,format=qcow2", o.Overlay),
		"-device", "ahci,id=ahci",
		"-device", "ide-hd,drive=osdisk,bus=ahci.0",
		"-netdev", "user,id=n0",
		"-device", "e1000,netdev=n0",
		// No -no-reboot: the golden may reboot once on its first runtime boot
		// (OOBE finalize) before the runner task fires. -no-reboot would make QEMU
		// exit on that reboot (~25s in) and the runner would never come online. A
		// guest power-off still exits QEMU without the flag, so the post-job
		// Stop-Computer still ends the VM and the backend reprovisions.
		// COM1 -> per-VM serial log for diagnosing boot/runner issues.
		"-serial", "file:"+o.Overlay+".serial.log",
	)
	// Debug: MULTIRUNNER_VM_VNC="0.0.0.0:2,websocket=5702" attaches a viewer + QMP.
	if vnc := os.Getenv("MULTIRUNNER_VM_VNC"); vnc != "" {
		args = append(args, "-vnc", vnc, "-qmp", "tcp:127.0.0.1:4457,server,nowait")
	} else {
		args = append(args, "-display", "none")
	}
	if o.JITISOPath != "" {
		args = append(args, "-drive", fmt.Sprintf("file=%s,media=cdrom,readonly=on", o.JITISOPath))
	}
	return args
}
