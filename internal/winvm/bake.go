package winvm

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/GerardSmit/multirunner/internal/vmview"
)

// DefaultRunnerVersion is the actions/runner baked into golden VMs. It is a
// single constant because the value is a hard dependency: GitHub rejects
// runners that are too old, and a rejected runner registers, idles briefly,
// then exits without claiming a job, which reads as a scheduling fault rather
// than a version one. Two copies of this literal previously drifted apart, so
// the CLI default and the option default must resolve to the same place.
//
// Keep this in step with RUNNER_VERSION in images/linux/Dockerfile and
// images/windows/Dockerfile.
const DefaultRunnerVersion = "2.337.0"

// minGitURL is the portable Git for Windows build staged into the golden so
// actions/checkout uses real git (incremental fetch + dotgit-cache bundle) and
// `run:`/job-hook steps can run git. Kept in sync with install-golden.ps1.
const minGitURL = "https://github.com/git-for-windows/git/releases/download/v2.54.0.windows.1/MinGit-2.54.0-64-bit.zip"

// Toolchain versions baked into the golden when requested via --tools. Kept here
// (not in the script) so the host can stage the big downloads onto the CD.
const (
	bakeNodeVersion = "22.20.0"
	bakeGoVersion   = "1.24.4"
)

func bakeNodeURL() string {
	return fmt.Sprintf("https://nodejs.org/dist/v%s/node-v%s-win-x64.zip", bakeNodeVersion, bakeNodeVersion)
}
func bakeGoURL() string {
	return fmt.Sprintf("https://go.dev/dl/go%s.windows-amd64.zip", bakeGoVersion)
}

// ToolsHash is a stable fingerprint of the requested toolchains, stored in the
// golden meta as the WorkflowsHash so housekeeping rebuilds the golden when the
// tool set changes.
func ToolsHash(tools []string) string {
	norm := normalizeTools(tools)
	if len(norm) == 0 {
		return ""
	}
	sum := sha256.Sum256([]byte(strings.Join(norm, ",")))
	return "tools:" + hex.EncodeToString(sum[:8])
}

// normalizeTools lowercases, de-dups, and sorts a tool list for stable hashing
// and substitution.
func normalizeTools(tools []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range tools {
		t = strings.ToLower(strings.TrimSpace(t))
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

//go:embed templates/autounattend.xml templates/install-golden.ps1 templates/startup.ps1 templates/setupcomplete.cmd templates/githook.ps1
var templatesFS embed.FS

// BakeOptions configures building (or rebuilding) a golden Windows image.
type BakeOptions struct {
	WindowsISO       string // path to a Windows Server ISO (Server Core selected by the answer file)
	Golden           string // output golden qcow2
	DiskGB           int
	MemMB            int
	CPUs             int
	Accel            string // "" = auto
	RunnerVersion    string
	Tools            []string // toolchains baked into the golden: dotnet | node | go | buildtools
	AdminPassword    string
	EvalDays         int  // 180 (server) / 90 (client)
	MaxRearms        int  // ~5
	Licensed         bool // a real key/KMS is configured -> skip eval housekeeping
	WorkflowsHash    string
	QEMUBin          string
	ImgBin           string
	OVMFCode         string        // UEFI firmware code (auto-detected if empty)
	OVMFVarsTemplate string        // UEFI vars template to copy
	VNCWeb           string        // if set (host:port), serve a noVNC viewer to watch the install
	Timeout          time.Duration // max install wall-clock before the bake kills a hung guest (default 45m)
}

func (o *BakeOptions) defaults() {
	if o.DiskGB <= 0 {
		o.DiskGB = 40
	}
	if o.MemMB <= 0 {
		o.MemMB = 4096
	}
	if o.CPUs <= 0 {
		o.CPUs = 2
	}
	if o.RunnerVersion == "" {
		o.RunnerVersion = DefaultRunnerVersion
	}
	if o.AdminPassword == "" {
		o.AdminPassword = "Multirunner!1"
	}
	if o.EvalDays <= 0 {
		o.EvalDays = 180
	}
	if o.MaxRearms <= 0 {
		o.MaxRearms = 5
	}
	if o.Timeout <= 0 {
		o.Timeout = 45 * time.Minute
		// Toolchains add large in-guest downloads/installs over the slow SLIRP
		// network; VS Build Tools especially. Give the install more headroom.
		for _, t := range normalizeTools(o.Tools) {
			switch t {
			case "buildtools":
				o.Timeout = 120 * time.Minute
			case "dotnet", "node", "go":
				if o.Timeout < 75*time.Minute {
					o.Timeout = 75 * time.Minute
				}
			}
		}
	}
	if o.Accel == "" {
		o.Accel = "" // resolved by caller/runtime; bake uses tcg fallback if empty
	}
	if o.QEMUBin == "" {
		o.QEMUBin = "qemu-system-x86_64"
	}
	if o.ImgBin == "" {
		o.ImgBin = "qemu-img"
	}
	if o.OVMFCode == "" {
		o.OVMFCode, o.OVMFVarsTemplate = DetectOVMF(o.QEMUBin)
	}
}

// GoldenVarsPath is the UEFI NVRAM file paired with a golden image.
func GoldenVarsPath(golden string) string { return golden + ".vars.fd" }

// GoldenSerialPath is the bake's COM1 capture file. install-golden.ps1 writes
// progress markers here (MR:...), ending with MR:GOLDEN_OK on success, so the
// host can verify the bake finished without mounting the guest disk.
func GoldenSerialPath(golden string) string { return golden + ".serial.log" }

// goldenOKMarker is the sentinel install-golden.ps1 writes to COM1 right before
// the final power-off. Its absence means the bake did not provision the runner.
const goldenOKMarker = "MR:GOLDEN_OK"

func copyFile(dst, src string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o644)
}

// AutounattendFiles returns the answer file + provisioning scripts with bake
// substitutions applied (for the autounattend ISO).
func AutounattendFiles(runnerVersion, adminPassword string, tools []string) (map[string]string, error) {
	read := func(name string) (string, error) {
		b, err := templatesFS.ReadFile("templates/" + name)
		return string(b), err
	}
	unattend, err := read("autounattend.xml")
	if err != nil {
		return nil, err
	}
	install, err := read("install-golden.ps1")
	if err != nil {
		return nil, err
	}
	startup, err := read("startup.ps1")
	if err != nil {
		return nil, err
	}
	setupComplete, err := read("setupcomplete.cmd")
	if err != nil {
		return nil, err
	}
	gitHook, err := read("githook.ps1")
	if err != nil {
		return nil, err
	}
	unattend = strings.ReplaceAll(unattend, "__ADMIN_PASSWORD__", adminPassword)
	install = strings.ReplaceAll(install, "__RUNNER_VERSION__", runnerVersion)
	install = strings.ReplaceAll(install, "__TOOLS__", strings.Join(normalizeTools(tools), ","))
	install = strings.ReplaceAll(install, "__NODE_URL__", bakeNodeURL())
	install = strings.ReplaceAll(install, "__GO_URL__", bakeGoURL())
	return map[string]string{
		"autounattend.xml":   unattend,
		"setupcomplete.cmd":  setupComplete,
		"githook.ps1":        gitHook,
		"install-golden.ps1": install,
		"startup.ps1":        startup,
	}, nil
}

// bakeQEMUArgs builds the installer-boot args: base disk + Windows ISO + the
// autounattend ISO, booting via UEFI (no "press any key" prompt). The guest
// installs Windows, runs install-golden.ps1, then powers off -> QEMU exits.
func bakeQEMUArgs(o BakeOptions, autounattendISO, varsFD string) []string {
	accel := o.Accel
	if accel == "" {
		accel = "tcg"
	}
	args := []string{
		"-machine", "q35",
		"-accel", accel,
		"-cpu", cpuArg(accel),
		"-m", strconv.Itoa(o.MemMB),
		"-smp", strconv.Itoa(o.CPUs),
	}
	if o.OVMFCode != "" {
		args = append(args,
			"-drive", "if=pflash,format=raw,unit=0,readonly=on,file="+o.OVMFCode,
			"-drive", "if=pflash,format=raw,unit=1,file="+varsFD)
	}
	args = append(args,
		"-drive", fmt.Sprintf("file=%s,if=none,id=osdisk,format=qcow2", o.Golden),
		"-device", "ahci,id=ahci",
		"-device", "ide-hd,drive=osdisk,bus=ahci.0",
		// Install CD carries a block-backend id so it can be ejected on the first
		// reset (bare media=cdrom, unchanged topology — an explicit AHCI device
		// crashes the first boot under WHPX). The autounattend CD stays for later.
		"-drive", fmt.Sprintf("file=%s,media=cdrom,id=%s", o.WindowsISO, bakeInstallCDDev),
		"-drive", fmt.Sprintf("file=%s,media=cdrom", autounattendISO),
		"-netdev", "user,id=n0",
		"-device", "e1000,netdev=n0",
		// No -boot d: forcing the DVD first means every Setup reboot re-enters the
		// CD bootloader, which triple-faults under WHPX ("Unexpected VP exit code
		// 4"). With default order, the empty HDD is skipped on first boot (so the
		// DVD installer still runs) but once Windows is applied the HDD is
		// bootable and reboots go straight to Windows Boot Manager.
		// NO -no-reboot: Windows Setup reboots several times mid-install; the
		// final Stop-Computer power-off is what exits QEMU.
		"-qmp", "tcp:"+bakeQMPAddr+",server,nowait",
		// COM1 -> file: install-golden.ps1 writes progress + the GOLDEN_OK
		// sentinel here so the host can verify the bake actually provisioned.
		"-serial", "file:"+GoldenSerialPath(o.Golden),
	)
	if o.VNCWeb != "" {
		args = append(args, "-vnc", fmt.Sprintf("0.0.0.0:%d,websocket=%d", bakeVNCDisplay, bakeVNCWSPort))
	} else {
		args = append(args, "-display", "none")
	}
	return args
}

const (
	// bakeQMPAddr is where the bake's QEMU exposes QMP (to dismiss the "press any
	// key to boot from CD" prompt via keypresses).
	bakeQMPAddr = "127.0.0.1:4455"
	// VNC display + websocket port for the optional noVNC viewer.
	bakeVNCDisplay = 1
	bakeVNCWSPort  = 5701
	// bakeInstallCDDev is the block-backend id of the Windows install CD, ejected
	// on the first guest reset so reboots boot the HDD instead of re-entering the
	// DVD UEFI loader (which triple-faults under WHPX).
	bakeInstallCDDev = "instcd"
)

// cpuArg picks the QEMU -cpu model for an accelerator. The bake and runtime must
// agree so the golden boots on the vCPU it was installed on.
//
// WHPX (Windows) gets Hyper-V enlightenments on a conservative qemu64 base:
// Windows then drives a paravirtual timer/APIC/spinlock interface instead of the
// hardware WHPX emulates poorly — without them the guest hangs at the
// specialize/OOBE boot. ("host" passthrough is NOT usable under WHPX here: it
// exposes APX, which makes OVMF #GP-fault in PlatformPei.) KVM/HVF use host
// passthrough; TCG uses the richest emulated model.
func cpuArg(accel string) string {
	switch accel {
	case "whpx":
		return "qemu64,hv-relaxed,hv-vapic,hv-spinlocks=0x1fff,hv-time,hv-synic,hv-stimer,hv-reset"
	case "tcg", "":
		return "max"
	default: // kvm, hvf
		return "host"
	}
}

// stageBakeBinaries downloads the runner + MinGit zips on the host (fast) into a
// temp dir and returns ISO file refs (name -> host path) plus a cleanup func.
// Best-effort: a file that fails to download is simply omitted, and the guest
// falls back to downloading it itself.
func stageBakeBinaries(ctx context.Context, runnerVersion string, tools []string) (map[string]string, func()) {
	dir, err := os.MkdirTemp("", "mr-bakestage")
	if err != nil {
		return nil, func() {}
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	refs := map[string]string{}
	runnerURL := fmt.Sprintf("https://github.com/actions/runner/releases/download/v%s/actions-runner-win-x64-%s.zip", runnerVersion, runnerVersion)
	stage := map[string]string{"runner.zip": runnerURL, "mingit.zip": minGitURL}
	// Stage the big single-file toolchain payloads on the CD too (SLIRP is slow).
	// vs_buildtools is not staged: its bootstrapper pulls payloads from MS at run
	// time regardless, so install-golden downloads it directly.
	for _, t := range normalizeTools(tools) {
		switch t {
		case "node":
			stage["node.zip"] = bakeNodeURL()
		case "go":
			stage["go.zip"] = bakeGoURL()
		case "dotnet":
			stage["dotnet-install.ps1"] = "https://dot.net/v1/dotnet-install.ps1"
		}
	}
	for name, url := range stage {
		dst := filepath.Join(dir, name)
		if err := downloadFile(ctx, url, dst); err != nil {
			continue
		}
		refs[name] = dst
	}
	return refs, cleanup
}

// downloadFile fetches url to dst.
func downloadFile(ctx context.Context, url, dst string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: status %d", url, resp.StatusCode)
	}
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, resp.Body)
	return err
}

// Prepare creates the base disk + autounattend ISO for a bake and returns the
// autounattend ISO path and the QEMU install args (without running QEMU).
func Prepare(ctx context.Context, o *BakeOptions) (autoISO string, args []string, err error) {
	o.defaults()
	if o.WindowsISO == "" || o.Golden == "" {
		return "", nil, fmt.Errorf("bake: WindowsISO and Golden are required")
	}
	if _, err := os.Stat(o.WindowsISO); err != nil {
		return "", nil, fmt.Errorf("bake: windows iso: %w", err)
	}
	if out, err := exec.CommandContext(ctx, o.ImgBin, "create", "-f", "qcow2", o.Golden, fmt.Sprintf("%dG", o.DiskGB)).CombinedOutput(); err != nil {
		return "", nil, fmt.Errorf("create golden disk: %w: %s", err, out)
	}
	_ = os.Remove(GoldenSerialPath(o.Golden)) // stale markers must not satisfy the bake check
	files, err := AutounattendFiles(o.RunnerVersion, o.AdminPassword, o.Tools)
	if err != nil {
		return "", nil, err
	}
	autoISO = o.Golden + ".autounattend.iso"
	// Stage the runner + MinGit zips onto the ISO so the guest reads them off the
	// virtual CD. The VM's user-mode (SLIRP) network is too slow/flaky for ~230MB
	// of downloads; the host fetches them fast and install-golden copies from CD,
	// falling back to a direct download only if a staged file is missing.
	refs, cleanup := stageBakeBinaries(ctx, o.RunnerVersion, o.Tools)
	defer cleanup()
	if err := BuildISOFiles(autoISO, "AUTOUNATTEND", files, refs); err != nil {
		return "", nil, err
	}

	// UEFI: create the golden's writable NVRAM from the vars template.
	varsFD := GoldenVarsPath(o.Golden)
	if o.OVMFCode != "" {
		if o.OVMFVarsTemplate == "" {
			return "", nil, fmt.Errorf("bake: UEFI code found but no vars template; set OVMFVarsTemplate")
		}
		if err := copyFile(varsFD, o.OVMFVarsTemplate); err != nil {
			return "", nil, fmt.Errorf("bake: create nvram: %w", err)
		}
	}
	return autoISO, bakeQEMUArgs(*o, autoISO, varsFD), nil
}

// Bake builds the golden image: create base disk, boot the unattended installer,
// wait for it to finish (guest powers off), and write the metadata sidecar.
func Bake(ctx context.Context, o BakeOptions, now time.Time) error {
	autoISO, args, err := Prepare(ctx, &o)
	if err != nil {
		return err
	}
	defer os.Remove(autoISO)

	cmd := exec.CommandContext(ctx, o.QEMUBin, args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start qemu: %w", err)
	}

	// Optional live viewer (noVNC) so the operator can watch the install.
	if o.VNCWeb != "" {
		viewCtx, cancelView := context.WithCancel(ctx)
		defer cancelView()
		logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
		fmt.Printf("\n  ┌─────────────────────────────────────────────────────────┐\n")
		fmt.Printf("  │  Watch the install live:  http://%-24s │\n", o.VNCWeb)
		fmt.Printf("  └─────────────────────────────────────────────────────────┘\n\n")
		go func() { _ = vmview.Serve(viewCtx, o.VNCWeb, bakeVNCWSPort, logger) }()
	}

	// Dismiss the "Press any key to boot from CD" prompt on first boot. QMP
	// keypresses are host-OS-agnostic (same path on Windows and Linux), so the
	// bake is exercised identically wherever it runs. (A no-prompt ISO is not an
	// option: Windows install media is UDF-primary with only a stub ISO9660 tree,
	// so the EFI boot files can't be located/replaced without a UDF reader.)
	go func() {
		time.Sleep(4 * time.Second)
		// First boot: dismiss "press any key to boot from CD" (QMPBootKeys closes
		// its QMP connection on return, freeing the single QMP socket).
		_ = QMPBootKeys(bakeQMPAddr, 20, time.Second)
		// Then eject the install CD on Setup's first reboot so it lands on the
		// HDD, avoiding the WHPX DVD-loader triple-fault.
		_ = QMPEjectOnReset(bakeQMPAddr, bakeInstallCDDev, 20*time.Minute)
	}()
	// Watchdog: WHPX can wedge the guest vCPU (frozen, no power-off) instead of
	// crashing — without a deadline the bake would wait forever. Kill QEMU if the
	// install hasn't powered off within the timeout.
	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait() }()
	select {
	case err := <-waitErr:
		if err != nil {
			return fmt.Errorf("golden install run: %w", err)
		}
	case <-time.After(o.Timeout):
		_ = cmd.Process.Kill()
		<-waitErr
		serial, _ := os.ReadFile(GoldenSerialPath(o.Golden))
		return fmt.Errorf("golden bake timed out after %s (guest hung); serial tail:\n%s",
			o.Timeout, serialTail(serial, 2000))
	}

	// Verify the guest actually provisioned the runner. The bake's QEMU exits on
	// any guest power-off, including ones that happen before install-golden runs
	// (e.g. an OOBE hiccup) — without this check a half-baked golden ships
	// silently. install-golden.ps1 writes MR:GOLDEN_OK to COM1 as its last step.
	serial, _ := os.ReadFile(GoldenSerialPath(o.Golden))
	if !strings.Contains(string(serial), goldenOKMarker) {
		return fmt.Errorf("golden bake did not complete: %q not found in serial log %s\n--- serial tail ---\n%s",
			goldenOKMarker, GoldenSerialPath(o.Golden), serialTail(serial, 2000))
	}

	meta := GoldenMeta{
		CreatedAt: now, EvalDays: o.EvalDays, MaxRearms: o.MaxRearms,
		Licensed: o.Licensed, WorkflowsHash: o.WorkflowsHash,
	}
	return SaveMeta(o.Golden, meta)
}

// serialTail returns the last n bytes of b as a string (for error context).
func serialTail(b []byte, n int) string {
	if len(b) > n {
		b = b[len(b)-n:]
	}
	return string(b)
}
