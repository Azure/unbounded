// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"
)

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
	BootTargetPxe      BootTarget = "Pxe"
	BootTargetHdd      BootTarget = "Hdd"
	BootTargetUefiHTTP BootTarget = "UefiHttp"
)

type BootEnabled string

const (
	BootContinuous BootEnabled = "Continuous"
	BootOnce       BootEnabled = "Once"
	BootDisabled   BootEnabled = "Disabled"
)

type BootMode string

const (
	BootModeUEFI BootMode = "UEFI"
)

type VMManager struct {
	cmd Commander
	cfg Config
	mu  sync.Mutex

	qemuProc  Process
	swtpmProc Process
	boot      BootConfig

	biosAttributes map[string]string
	nicStatic      NICStaticConfig
}

type BootConfig struct {
	Target      BootTarget
	Enabled     BootEnabled
	Mode        BootMode
	HTTPBootURI string
}

// NICStaticConfig records the last static IPv4 settings applied to the guest
// EthernetInterface via Redfish. It is stored for observability and testing;
// the emulator does not reconfigure the running guest from it.
type NICStaticConfig struct {
	DHCPEnabled bool
	Address     string
	SubnetMask  string
	Gateway     string
	NameServers []string
	Applied     bool
}

func NewVMManager(cmd Commander, cfg Config) *VMManager {
	return &VMManager{
		cmd: cmd,
		cfg: cfg,
		boot: BootConfig{
			Target:  BootTargetPxe,
			Enabled: BootContinuous,
		},
		biosAttributes: map[string]string{},
	}
}

func (m *VMManager) PowerState() PowerState {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.qemuProc == nil || m.qemuProc.Exited() {
		return PowerOff
	}

	return PowerOn
}

func (m *VMManager) BootConfig() BootConfig {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.boot
}

func (m *VMManager) SetBootConfig(config BootConfig) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if config.Target != "" {
		m.boot.Target = config.Target
	}

	if config.Enabled != "" {
		m.boot.Enabled = config.Enabled
	}

	if config.Mode != "" {
		m.boot.Mode = config.Mode
	}

	if config.HTTPBootURI != "" {
		m.boot.HTTPBootURI = config.HTTPBootURI
	}
}

// BIOSAttributes returns a copy of the pending BIOS settings applied via Redfish.
func (m *VMManager) BIOSAttributes() map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make(map[string]string, len(m.biosAttributes))
	for k, v := range m.biosAttributes {
		out[k] = v
	}

	return out
}

// SetBIOSAttributes merges the given pending BIOS settings.
func (m *VMManager) SetBIOSAttributes(attributes map[string]string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for k, v := range attributes {
		m.biosAttributes[k] = v
	}
}

// NICStatic returns the last static IPv4 settings applied to the guest NIC.
func (m *VMManager) NICStatic() NICStaticConfig {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.nicStatic
}

// SetNICStatic records static IPv4 settings applied to the guest NIC.
func (m *VMManager) SetNICStatic(config NICStaticConfig) {
	m.mu.Lock()
	defer m.mu.Unlock()

	config.Applied = true
	m.nicStatic = config
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

	if m.qemuProc != nil && !m.qemuProc.Exited() {
		return nil
	}

	if err := os.MkdirAll(m.cfg.DataDir, 0o755); err != nil {
		return err
	}

	if err := m.ensureDisk(ctx); err != nil {
		return err
	}

	if err := m.ensureOVMFVars(); err != nil {
		return err
	}

	if m.cfg.QEMU.EnableTPM {
		swtpmProc, err := m.startSWTPM(context.WithoutCancel(ctx))
		if err != nil {
			return err
		}

		m.swtpmProc = swtpmProc
	}

	args := m.qemuArgs()

	proc, err := m.cmd.Start(context.WithoutCancel(ctx), m.cfg.QEMU.Binary, args, filepath.Join(m.cfg.DataDir, "qemu.log"), filepath.Join(m.cfg.DataDir, "qemu.err"))
	if err != nil {
		err = errors.Join(err, m.stopProcess(m.swtpmProc))
		m.swtpmProc = nil

		return err
	}

	m.qemuProc = proc

	return nil
}

func (m *VMManager) Stop() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	var errs []error
	if err := m.stopProcess(m.qemuProc); err != nil {
		errs = append(errs, err)
	}

	if err := m.stopProcess(m.swtpmProc); err != nil {
		errs = append(errs, err)
	}

	m.qemuProc = nil
	m.swtpmProc = nil

	return errors.Join(errs...)
}

func (m *VMManager) ensureDisk(ctx context.Context) error {
	disk := filepath.Join(m.cfg.DataDir, "disk.qcow2")
	if _, err := os.Stat(disk); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	return m.cmd.Run(ctx, m.cfg.QEMU.ImgBinary, "create", "-f", "qcow2", disk, m.cfg.QEMU.DiskSize)
}

func (m *VMManager) ensureOVMFVars() error {
	vars := filepath.Join(m.cfg.DataDir, "OVMF_VARS.fd")
	if _, err := os.Stat(vars); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	data, err := os.ReadFile(m.cfg.QEMU.OVMFVarsTemplate)
	if err != nil {
		return err
	}

	return os.WriteFile(vars, data, 0o600)
}

func (m *VMManager) startSWTPM(ctx context.Context) (Process, error) {
	tpmDir := filepath.Join(os.TempDir(), "playpen-runner-tpm")
	if err := os.MkdirAll(tpmDir, 0o700); err != nil {
		return nil, err
	}

	socketPath := m.swtpmSocketPath()
	if err := os.Remove(socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	args := []string{
		"socket",
		"--tpm2",
		"--runas", "0",
		"--tpmstate", "dir=" + tpmDir,
		"--ctrl", "type=unixio,path=" + socketPath,
		"--log", "level=20",
	}

	proc, err := m.cmd.Start(ctx, m.cfg.QEMU.SWTPMBinary, args, filepath.Join(m.cfg.DataDir, "swtpm.log"), filepath.Join(m.cfg.DataDir, "swtpm.err"))
	if err != nil {
		return nil, err
	}

	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	if err := waitForUnixSocket(waitCtx, socketPath); err != nil {
		return nil, errors.Join(err, m.stopProcess(proc))
	}

	return proc, nil
}

func (m *VMManager) swtpmSocketPath() string {
	return filepath.Join(os.TempDir(), "playpen-runner-swtpm.sock")
}

func waitForUnixSocket(ctx context.Context, path string) error {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()

	for {
		info, err := os.Stat(path)
		if err == nil {
			if info.Mode()&os.ModeSocket == 0 {
				return fmt.Errorf("%s exists but is not a Unix socket", path)
			}

			return nil
		}

		if !errors.Is(err, os.ErrNotExist) {
			return err
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for Unix socket %s: %w", path, ctx.Err())
		case <-ticker.C:
		}
	}
}

func (m *VMManager) qemuArgs() []string {
	// Network boot ("n") covers both PXE and UEFI HTTP boot targets, which are
	// both network-based. Disk boot ("c") is used when the override is disabled
	// or explicitly targets the local disk.
	bootOrder := "n"
	if m.boot.Enabled == BootDisabled || m.boot.Target == BootTargetHdd {
		bootOrder = "c"
	}

	args := []string{
		"-enable-kvm",
		"-machine", m.cfg.QEMU.Machine,
		"-cpu", m.cfg.QEMU.CPU,
		"-m", strconv.Itoa(m.cfg.QEMU.MemoryMiB),
		"-smp", strconv.Itoa(m.cfg.QEMU.CPUs),
		"-drive", "if=pflash,format=raw,readonly=on,file=" + m.cfg.QEMU.OVMFCodeFile,
		"-drive", "if=pflash,format=raw,file=" + filepath.Join(m.cfg.DataDir, "OVMF_VARS.fd"),
		"-drive", "file=" + filepath.Join(m.cfg.DataDir, "disk.qcow2") + ",format=qcow2,if=virtio",
		"-netdev", "tap,id=net0,ifname=" + m.cfg.TapName + ",script=no,downscript=no",
		"-device", m.cfg.QEMU.NICDevice + ",netdev=net0,mac=" + m.cfg.Guest.MAC,
		"-boot", "order=" + bootOrder,
		"-serial", "file:" + filepath.Join(m.cfg.DataDir, "serial.log"),
		"-chardev", "socket,path=" + filepath.Join(m.cfg.DataDir, "qga.sock") + ",server=on,wait=off,id=qga0",
		"-device", m.cfg.QEMU.SerialDevice,
		"-device", "virtserialport,chardev=qga0,name=org.qemu.guest_agent.0",
		"-display", "none",
	}

	if m.cfg.QEMU.EnableTPM {
		args = append(args,
			"-chardev", "socket,id=chrtpm,path="+m.swtpmSocketPath(),
			"-tpmdev", "emulator,id=tpm0,chardev=chrtpm",
			"-device", m.cfg.QEMU.TPMDevice+",tpmdev=tpm0",
		)
	}

	return args
}

func (m *VMManager) stopProcess(proc Process) error {
	if proc == nil || proc.Exited() {
		return nil
	}

	if err := proc.Signal(syscall.SIGTERM); err != nil {
		return err
	}

	done := make(chan error, 1)

	go func() { done <- proc.Wait() }()

	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		if err := proc.Kill(); err != nil {
			return err
		}

		return <-done
	}
}
