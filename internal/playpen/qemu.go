// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package playpen

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	qemuTapFD             = 3
	qemuShutdownGraceTime = 10 * time.Second
)

// Firmware identifies the read-only UEFI code image and writable vars image
// passed to QEMU. Empty paths mean firmware arguments are omitted.
type Firmware struct {
	CodePath string
	VarsPath string
}

type PowerState string

const (
	PowerOn  PowerState = "On"
	PowerOff PowerState = "Off"
)

type ResetType string

const (
	ResetForceOff ResetType = "ForceOff"
	ResetOn       ResetType = "On"
)

type BootTarget string

const (
	BootTargetPxe BootTarget = "Pxe"
	BootTargetHdd BootTarget = "Hdd"
)

type BootEnabled string

const (
	BootContinuous BootEnabled = "Continuous"
	BootOnce       BootEnabled = "Once"
	BootDisabled   BootEnabled = "Disabled"
)

type BootConfig struct {
	Target  BootTarget
	Enabled BootEnabled
}

type tpmProcess interface {
	Close() error
}

type qemuProcess struct {
	cmd  *exec.Cmd
	done chan struct{}
}

// VMManager serializes Redfish power actions and owns both QEMU and its swtpm.
type VMManager struct {
	cfg           Config
	tapFile       *os.File
	firmware      Firmware
	buildCommand  commandBuilder
	startTPM      func(context.Context, Config) (tpmProcess, error)
	shutdownGrace time.Duration

	mu   sync.Mutex
	qemu *qemuProcess
	tpm  tpmProcess
	boot BootConfig
}

func NewVMManager(cfg Config, tapFile *os.File, firmware Firmware) *VMManager {
	return newVMManager(cfg, tapFile, firmware, exec.Command, func(ctx context.Context, cfg Config) (tpmProcess, error) {
		return StartSWTPM(ctx, cfg)
	}, qemuShutdownGraceTime)
}

func newVMManager(
	cfg Config,
	tapFile *os.File,
	firmware Firmware,
	buildCommand commandBuilder,
	startTPM func(context.Context, Config) (tpmProcess, error),
	shutdownGrace time.Duration,
) *VMManager {
	return &VMManager{
		cfg:           cfg,
		tapFile:       tapFile,
		firmware:      firmware,
		buildCommand:  buildCommand,
		startTPM:      startTPM,
		shutdownGrace: shutdownGrace,
		boot: BootConfig{
			Target:  BootTargetPxe,
			Enabled: BootContinuous,
		},
	}
}

func validateKVM(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}

	if info.IsDir() || info.Mode()&os.ModeDevice == 0 || info.Mode()&os.ModeCharDevice == 0 {
		return fmt.Errorf("%s is not a KVM character device", path)
	}

	return nil
}

func PrepareFirmware(cfg Config) (Firmware, error) {
	codePath, varsTemplate, err := resolveFirmwarePaths(cfg)
	if err != nil {
		return Firmware{}, err
	}

	if codePath == "" {
		return Firmware{}, nil
	}

	if _, err := os.Stat(codePath); err != nil {
		return Firmware{}, fmt.Errorf("stat uefi code %s: %w", codePath, err)
	}

	firmware := Firmware{CodePath: codePath}
	if varsTemplate == "" {
		return firmware, nil
	}

	if _, err := os.Stat(varsTemplate); err != nil {
		return Firmware{}, fmt.Errorf("stat uefi vars %s: %w", varsTemplate, err)
	}

	if err := os.MkdirAll(cfg.RuntimeDir, 0o755); err != nil {
		return Firmware{}, fmt.Errorf("create runtime dir %s: %w", cfg.RuntimeDir, err)
	}

	varsPath := filepath.Join(cfg.RuntimeDir, firmwareVarsFileName(cfg.Arch))
	if err := copyFile(varsTemplate, varsPath); err != nil {
		return Firmware{}, err
	}

	firmware.VarsPath = varsPath

	return firmware, nil
}

// PrepareDisk creates the sparse raw guest disk when it does not yet contain
// data. Existing disks are left unchanged so installed guest state persists.
func PrepareDisk(cfg Config) (retErr error) {
	size, err := parseDiskSize(cfg.DiskSize)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(cfg.DiskPath), 0o755); err != nil {
		return fmt.Errorf("create disk directory for %s: %w", cfg.DiskPath, err)
	}

	disk, err := os.OpenFile(cfg.DiskPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("open guest disk %s: %w", cfg.DiskPath, err)
	}

	defer func() {
		if err := disk.Close(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close guest disk %s: %w", cfg.DiskPath, err))
		}
	}()

	info, err := disk.Stat()
	if err != nil {
		return fmt.Errorf("stat guest disk %s: %w", cfg.DiskPath, err)
	}

	if !info.Mode().IsRegular() {
		return fmt.Errorf("guest disk %s is not a regular file", cfg.DiskPath)
	}

	if info.Size() != 0 {
		return nil
	}

	if err := disk.Truncate(size); err != nil {
		return fmt.Errorf("size guest disk %s: %w", cfg.DiskPath, err)
	}

	return nil
}

func BuildQEMUArgs(cfg Config, tapFD int, firmware Firmware) ([]string, error) {
	return buildQEMUArgs(cfg, tapFD, firmware, BootConfig{Target: BootTargetPxe, Enabled: BootContinuous})
}

func buildQEMUArgs(cfg Config, tapFD int, firmware Firmware, boot BootConfig) ([]string, error) {
	mac, err := ParseMAC(cfg.MAC)
	if err != nil {
		return nil, err
	}

	args := []string{
		"-name", cfg.Name,
		"-enable-kvm",
	}

	var tpmDevice string

	switch cfg.Arch {
	case ArchAMD64:
		tpmDevice = "tpm-tis,tpmdev=tpm0"

		args = append(args,
			"-machine", "q35,accel=kvm",
			"-cpu", "host",
		)
	case ArchARM64:
		tpmDevice = "tpm-tis-device,tpmdev=tpm0"

		args = append(args,
			"-machine", "virt,accel=kvm,gic-version=host",
			"-cpu", "host",
		)
	default:
		return nil, fmt.Errorf("unsupported arch %q", cfg.Arch)
	}

	if firmware.CodePath != "" {
		args = append(args,
			"-drive", fmt.Sprintf("if=pflash,format=raw,readonly=on,file=%s", firmware.CodePath),
		)
	}

	if firmware.VarsPath != "" {
		args = append(args,
			"-drive", fmt.Sprintf("if=pflash,format=raw,file=%s", firmware.VarsPath),
		)
	}

	networkBootIndex, diskBootIndex := 1, 2
	if boot.Enabled == BootDisabled || boot.Target == BootTargetHdd {
		networkBootIndex, diskBootIndex = 2, 1
	}

	args = append(args,
		"-chardev", fmt.Sprintf("socket,id=chrtpm,path=%s", escapeQEMUOption(cfg.TPMSocket)),
		"-tpmdev", "emulator,id=tpm0,chardev=chrtpm",
		"-device", tpmDevice,
		"-drive", fmt.Sprintf("if=none,id=disk0,format=raw,file=%s", escapeQEMUOption(cfg.DiskPath)),
		"-device", fmt.Sprintf("virtio-blk-pci,drive=disk0,bootindex=%d", diskBootIndex),
	)

	args = append(args,
		"-m", cfg.Memory,
		"-smp", fmt.Sprintf("%d", cfg.CPUs),
		"-nodefaults",
		"-no-user-config",
		"-serial", "mon:stdio",
		"-display", "none",
		"-boot", "menu=on,strict=on",
		"-netdev", fmt.Sprintf("tap,id=net0,fd=%d", tapFD),
		"-device", fmt.Sprintf("virtio-net-pci,netdev=net0,mac=%s,bootindex=%d", mac.String(), networkBootIndex),
	)

	args = append(args, cfg.ExtraQEMUArgs...)

	return args, nil
}

func escapeQEMUOption(value string) string {
	return strings.ReplaceAll(value, ",", ",,")
}

func (m *VMManager) PowerState() PowerState {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.qemu == nil || processExited(m.qemu) {
		return PowerOff
	}

	return PowerOn
}

func (m *VMManager) BootConfig() BootConfig {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.boot
}

func (m *VMManager) SetBootConfig(boot BootConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	next := m.boot

	if boot.Target != "" {
		switch boot.Target {
		case BootTargetPxe, BootTargetHdd:
			next.Target = boot.Target
		default:
			return fmt.Errorf("unsupported boot target %q", boot.Target)
		}
	}

	if boot.Enabled != "" {
		switch boot.Enabled {
		case BootContinuous, BootOnce, BootDisabled:
			next.Enabled = boot.Enabled
		default:
			return fmt.Errorf("unsupported boot override mode %q", boot.Enabled)
		}
	}

	m.boot = next

	return nil
}

func (m *VMManager) Reset(ctx context.Context, reset ResetType) error {
	switch reset {
	case ResetOn:
		return m.Start(ctx)
	case ResetForceOff:
		return m.Stop()
	default:
		return fmt.Errorf("unsupported reset type %q", reset)
	}
}

func (m *VMManager) Start(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.qemu != nil && !processExited(m.qemu) {
		return nil
	}

	if err := m.clearExitedLocked(); err != nil {
		return err
	}

	tpm, err := m.startTPM(context.WithoutCancel(ctx), m.cfg)
	if err != nil {
		return err
	}

	args, err := buildQEMUArgs(m.cfg, qemuTapFD, m.firmware, m.boot)
	if err != nil {
		return errors.Join(err, tpm.Close())
	}

	cmd := m.buildCommand(m.cfg.QEMUBinary, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.ExtraFiles = []*os.File{m.tapFile}

	if err := cmd.Start(); err != nil {
		return errors.Join(fmt.Errorf("start %s: %w", m.cfg.QEMUBinary, err), tpm.Close())
	}

	process := &qemuProcess{cmd: cmd, done: make(chan struct{})}
	m.qemu = process
	m.tpm = tpm

	if m.boot.Enabled == BootOnce {
		m.boot.Enabled = BootDisabled
	}

	go m.wait(process)

	return nil
}

func (m *VMManager) Stop() error {
	m.mu.Lock()

	if m.qemu == nil || processExited(m.qemu) {
		err := m.clearExitedLocked()
		m.mu.Unlock()

		return err
	}

	process := m.qemu
	if err := process.cmd.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		if killErr := killProcess(process.cmd.Process); killErr != nil {
			m.mu.Unlock()

			return errors.Join(fmt.Errorf("signal qemu: %w", err), fmt.Errorf("kill qemu: %w", killErr))
		}
	}

	timer := time.NewTimer(m.shutdownGrace)
	defer timer.Stop()

	select {
	case <-process.done:
	case <-timer.C:
		if err := killProcess(process.cmd.Process); err != nil {
			m.mu.Unlock()

			return fmt.Errorf("kill qemu after shutdown timeout: %w", err)
		}

		<-process.done
	}

	err := m.clearExitedLocked()
	m.mu.Unlock()

	return err
}

func (m *VMManager) Close() error {
	return m.Stop()
}

func (m *VMManager) wait(process *qemuProcess) {
	process.cmd.Wait() //nolint:errcheck // Power state is derived from process exit, regardless of status.
	close(process.done)

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.qemu != process {
		return
	}

	tpm := m.tpm
	m.qemu = nil
	m.tpm = nil

	if tpm != nil {
		if err := tpm.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "warning: stop swtpm after QEMU exit: %v\n", err)
		}
	}
}

func (m *VMManager) clearExitedLocked() error {
	if m.qemu != nil && !processExited(m.qemu) {
		return nil
	}

	tpm := m.tpm
	m.qemu = nil
	m.tpm = nil

	if tpm != nil {
		return tpm.Close()
	}

	return nil
}

func processExited(process *qemuProcess) bool {
	select {
	case <-process.done:
		return true
	default:
		return false
	}
}

func killProcess(process *os.Process) error {
	if err := process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}

	return nil
}

func resolveFirmwarePaths(cfg Config) (string, string, error) {
	defaults, err := defaultFirmwarePaths(cfg.Arch)
	if err != nil {
		return "", "", err
	}

	codePath := firstExistingPath(defaults.code)
	if codePath == "" {
		codePath = defaults.code[0]
	}

	varsPath := firstExistingPath(defaults.vars)
	if varsPath == "" {
		varsPath = defaults.vars[0]
	}

	if cfg.UEFICode != "" {
		codePath = cfg.UEFICode
	}

	if cfg.UEFIVars != "" {
		varsPath = cfg.UEFIVars
	}

	return codePath, varsPath, nil
}

type firmwareDefaults struct {
	code []string
	vars []string
}

func defaultFirmwarePaths(arch string) (firmwareDefaults, error) {
	switch arch {
	case ArchAMD64:
		return firmwareDefaults{
			code: []string{
				"/usr/share/OVMF/OVMF_CODE_4M.fd",
				"/usr/share/OVMF/OVMF_CODE.fd",
			},
			vars: []string{
				"/usr/share/OVMF/OVMF_VARS_4M.fd",
				"/usr/share/OVMF/OVMF_VARS.fd",
			},
		}, nil
	case ArchARM64:
		return firmwareDefaults{
			code: []string{
				"/usr/share/AAVMF/AAVMF_CODE.fd",
				"/usr/share/AAVMF/AAVMF_CODE.ms.fd",
			},
			vars: []string{
				"/usr/share/AAVMF/AAVMF_VARS.fd",
				"/usr/share/AAVMF/AAVMF_VARS.ms.fd",
			},
		}, nil
	default:
		return firmwareDefaults{}, fmt.Errorf("unsupported arch %q", arch)
	}
}

func firstExistingPath(paths []string) string {
	for _, path := range paths {
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}

	return ""
}

func firmwareVarsFileName(arch string) string {
	switch arch {
	case ArchAMD64:
		return "OVMF_VARS.fd"
	case ArchARM64:
		return "AAVMF_VARS.fd"
	default:
		return "UEFI_VARS.fd"
	}
}

func copyFile(src, dst string) (retErr error) {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}

	defer func() {
		if err := in.Close(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close %s: %w", src, err))
		}
	}()

	out, err := os.OpenFile(dst, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("create %s: %w", dst, err)
	}

	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()

	if copyErr != nil {
		return fmt.Errorf("copy %s to %s: %w", src, dst, copyErr)
	}

	if closeErr != nil {
		return fmt.Errorf("close %s: %w", dst, closeErr)
	}

	return nil
}
