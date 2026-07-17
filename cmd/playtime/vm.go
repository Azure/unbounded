// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/vishvananda/netlink"
)

// chBinary is the cloud-hypervisor VMM launched for the guest. It is a var so
// tests can observe the constructed argument vector without execing.
var chBinary = "cloud-hypervisor"

// chFirmware is the UEFI firmware cloud-hypervisor boots. It is an EDK2 CLOUDHV
// build that includes the UEFI network stack so the diskless guest can PXE
// network-boot over the overlay (cloud-hypervisor has no legacy option-ROM
// netboot path). It is a var so tests can observe the constructed argument
// vector without a firmware file being present.
var chFirmware = "/usr/share/cloud-hypervisor/CLOUDHV.fd"

// swtpmBinary is the software TPM emulator backing the guest's emulated TPM. It
// is a var so tests can observe behaviour without execing. cloud-hypervisor
// connects to it over the swtpm control socket (tpmSocketPath).
var swtpmBinary = "swtpm"

// vmStateDir holds the cloud-hypervisor API socket, serial log, backing disk,
// and swtpm state/socket for the pod's guest.
const vmStateDir = "/tmp/playtime-vm"

// tpmSocketPath is the swtpm control socket cloud-hypervisor connects to for the
// guest's emulated TPM.
func tpmSocketPath() string {
	return filepath.Join(vmStateDir, "tpm.sock")
}

// setupVMBridge wires the pod side of the overlay for a guest VM. It creates a
// Linux bridge, enslaves the VXLAN device, and adds a tap device for the guest
// NIC so the VXLAN and the guest share a single L2 segment. The pod overlay
// address is carried on the bridge (rather than the VXLAN device) so the pod
// remains the guest's gateway and NAT egress point while bridging guest frames
// onto the overlay.
//
// It assumes configureServer has already created and configured the VXLAN
// device (with the overlay address assigned to it); this function removes that
// address from the VXLAN device and moves it to the bridge.
func setupVMBridge(cfg Config) error {
	vxlan, err := netlink.LinkByName(cfg.VXLANInterface)
	if err != nil {
		return fmt.Errorf("look up vxlan %q: %w", cfg.VXLANInterface, err)
	}

	// Recreate the bridge so repeated runs converge to a known state.
	if existing, err := netlink.LinkByName(cfg.BridgeInterface); err == nil {
		if err := netlink.LinkDel(existing); err != nil {
			return fmt.Errorf("delete existing bridge %q: %w", cfg.BridgeInterface, err)
		}
	}

	bridge := &netlink.Bridge{
		LinkAttrs: netlink.LinkAttrs{
			Name: cfg.BridgeInterface,
			MTU:  cfg.OverlayMTU,
		},
	}
	if err := netlink.LinkAdd(bridge); err != nil {
		return fmt.Errorf("add bridge %q: %w", cfg.BridgeInterface, err)
	}

	// Move the overlay address from the VXLAN device to the bridge.
	addr, err := netlink.ParseAddr(fmt.Sprintf("%s/%d", cfg.OverlayRemoteIP, cfg.OverlayPrefix))
	if err != nil {
		return fmt.Errorf("parse overlay address %s/%d: %w", cfg.OverlayRemoteIP, cfg.OverlayPrefix, err)
	}

	if err := netlink.AddrDel(vxlan, addr); err != nil {
		return fmt.Errorf("remove overlay address %s from %q: %w", addr, cfg.VXLANInterface, err)
	}

	if err := netlink.AddrAdd(bridge, addr); err != nil {
		return fmt.Errorf("add overlay address %s to bridge %q: %w", addr, cfg.BridgeInterface, err)
	}

	// Enslave the VXLAN device to the bridge.
	if err := netlink.LinkSetMaster(vxlan, bridge); err != nil {
		return fmt.Errorf("enslave %q to bridge %q: %w", cfg.VXLANInterface, cfg.BridgeInterface, err)
	}

	// Recreate the tap device for the guest NIC and enslave it to the bridge.
	if existing, err := netlink.LinkByName(cfg.TapInterface); err == nil {
		if err := netlink.LinkDel(existing); err != nil {
			return fmt.Errorf("delete existing tap %q: %w", cfg.TapInterface, err)
		}
	}

	tap := &netlink.Tuntap{
		LinkAttrs: netlink.LinkAttrs{
			Name: cfg.TapInterface,
			MTU:  cfg.OverlayMTU,
		},
		Mode: netlink.TUNTAP_MODE_TAP,
	}
	if err := netlink.LinkAdd(tap); err != nil {
		return fmt.Errorf("add tap %q: %w", cfg.TapInterface, err)
	}

	if err := netlink.LinkSetMaster(tap, bridge); err != nil {
		return fmt.Errorf("enslave tap %q to bridge %q: %w", cfg.TapInterface, cfg.BridgeInterface, err)
	}

	if err := netlink.LinkSetUp(tap); err != nil {
		return fmt.Errorf("set tap %q up: %w", cfg.TapInterface, err)
	}

	if err := netlink.LinkSetUp(bridge); err != nil {
		return fmt.Errorf("set bridge %q up: %w", cfg.BridgeInterface, err)
	}

	return nil
}

// Power states reported by the guest, matching the Redfish PowerState values
// metalman consumes.
const (
	powerStateOn  = "On"
	powerStateOff = "Off"
)

// ResetType is a Redfish ComputerSystem.Reset action value. Only the subset
// metalman drives for power on/off is supported.
type ResetType string

const (
	resetOn           ResetType = "On"
	resetForceOff     ResetType = "ForceOff"
	resetForceRestart ResetType = "ForceRestart"
)

// cloud-hypervisor guest configuration (JSON body for PUT /api/v1/vm.create).
// Only the fields the diskless PXE guest needs are modelled.
type chVMConfig struct {
	Cpus    chCPUs    `json:"cpus"`
	Memory  chMemory  `json:"memory"`
	Payload chPayload `json:"payload"`
	Disks   []chDisk  `json:"disks,omitempty"`
	Net     []chNet   `json:"net"`
	Serial  chConsole `json:"serial"`
	Console chConsole `json:"console"`
	Tpm     *chTPM    `json:"tpm,omitempty"`
}

// chTPM attaches an emulated TPM 2.0 to the guest. cloud-hypervisor speaks the
// swtpm control protocol over the given unix socket, and surfaces the device to
// the guest as a TPM CRB so the installed OS exposes /dev/tpm0 and /dev/tpmrm0.
// The unbounded-agent's attestation phase requires the resource-manager device
// /dev/tpmrm0, so the guest cannot join without a TPM.
type chTPM struct {
	Socket string `json:"socket"`
}

// chDisk is a raw block device backing the guest. ImageType is set explicitly
// to "raw" because cloud-hypervisor auto-detects raw images and then disables
// writes to sector 0 (a partition-table safety guard), which would reject the
// installer's GPT write and leave the disk without a root partition.
type chDisk struct {
	Path      string `json:"path"`
	ImageType string `json:"image_type,omitempty"`
}

type chCPUs struct {
	BootVcpus int `json:"boot_vcpus"`
	MaxVcpus  int `json:"max_vcpus"`
}

type chMemory struct {
	Size int64 `json:"size"`
}

type chPayload struct {
	Firmware string `json:"firmware"`
}

type chNet struct {
	Tap string `json:"tap"`
	Mac string `json:"mac"`
}

type chConsole struct {
	Mode string `json:"mode"`
	File string `json:"file,omitempty"`
}

// chVMInfo is the subset of GET /api/v1/vm.info this code inspects.
type chVMInfo struct {
	State string `json:"state"`
}

// vmConfig builds the cloud-hypervisor guest configuration. The guest PXE
// network-boots through its single virtio NIC, which is attached to the
// pre-created tap device (already bridged onto the overlay). Boot proceeds
// through the UEFI firmware (chFirmware), whose network stack netboots off the
// NIC. When a disk is configured (cfg.VMDiskSizeGiB > 0) it is attached as a
// raw virtio block device so the netboot installer can write an OS image to it
// and the guest can subsequently boot the installed OS from disk; the firmware
// falls through to network boot while the disk is blank. Serial output is
// written to a log for debugging and the console is disabled since the pod is
// headless.
func vmConfig(cfg Config, serialLog string) chVMConfig {
	c := chVMConfig{
		Cpus: chCPUs{
			BootVcpus: cfg.VMCPUs,
			MaxVcpus:  cfg.VMCPUs,
		},
		Memory: chMemory{
			Size: int64(cfg.VMMemoryMiB) * 1024 * 1024,
		},
		Payload: chPayload{
			Firmware: chFirmware,
		},
		Net: []chNet{{
			Tap: cfg.TapInterface,
			Mac: cfg.VMMAC,
		}},
		Serial: chConsole{
			Mode: "File",
			File: serialLog,
		},
		Console: chConsole{
			Mode: "Off",
		},
		Tpm: &chTPM{
			Socket: tpmSocketPath(),
		},
	}

	if cfg.VMDiskSizeGiB > 0 {
		c.Disks = []chDisk{{Path: cfg.diskPath(), ImageType: "Raw"}}
	}

	return c
}

// vmManager owns a cloud-hypervisor VMM process and drives the guest's power
// state through the VMM's REST API over a unix socket. The VMM is launched with
// no guest configured (powered off); the guest is created and booted on demand
// by the in-pod Redfish server. cloud-hypervisor is KVM-only, so the pod must
// run on a KVM-capable node.
type vmManager struct {
	cfg       Config
	cmd       *exec.Cmd
	socket    string
	serialLog string
	client    *http.Client
}

// startVM sets up the bridge/tap wiring and launches a cloud-hypervisor VMM
// bound to ctx with only its API socket open (no guest booted). The process is
// terminated when ctx is cancelled (for example when the pod receives SIGTERM),
// keeping the guest lifecycle tied to the server process. The returned manager
// controls guest power via the API socket.
func startVM(ctx context.Context, cfg Config) (*vmManager, error) {
	if err := setupVMBridge(cfg); err != nil {
		return nil, err
	}

	stateDir := vmStateDir
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return nil, fmt.Errorf("create vm state dir: %w", err)
	}

	// Start the software TPM the guest attests with. cloud-hypervisor connects
	// to its control socket at vm.create, so it must be listening first.
	if err := startSwtpm(ctx, stateDir); err != nil {
		return nil, err
	}

	// Provision the guest's backing disk when configured. A sparse raw file is
	// created once and reused for the pod's lifetime, so an OS image written by
	// the netboot installer persists across guest reboots within a run. An
	// existing file is left untouched so power cycles keep the installed OS.
	if cfg.VMDiskSizeGiB > 0 {
		diskPath := cfg.diskPath()
		if _, err := os.Stat(diskPath); os.IsNotExist(err) {
			if err := createDisk(diskPath, cfg.VMDiskSizeGiB); err != nil {
				return nil, err
			}
		} else if err != nil {
			return nil, fmt.Errorf("stat vm disk %q: %w", diskPath, err)
		}
	}

	socket := filepath.Join(stateDir, "api.sock")

	// Remove any stale socket so cloud-hypervisor can bind cleanly on restart.
	if err := os.Remove(socket); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("remove stale api socket %q: %w", socket, err)
	}

	cmd := exec.CommandContext(ctx, chBinary, "--api-socket", socket)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start cloud-hypervisor (%s): %w", chBinary, err)
	}

	m := &vmManager{
		cfg:       cfg,
		cmd:       cmd,
		socket:    socket,
		serialLog: filepath.Join(stateDir, "serial.log"),
		client:    unixHTTPClient(socket),
	}

	if err := m.waitReady(ctx, 10*time.Second); err != nil {
		return nil, err
	}

	return m, nil
}

// createDisk creates a sparse raw disk image of the given size (in GiB) at
// path. The file is truncated to its full logical size but occupies no blocks
// until written, so it is cheap to create.
func createDisk(path string, sizeGiB int) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create vm disk %q: %w", path, err)
	}
	defer f.Close() //nolint:errcheck // closed explicitly below; deferred close is a backstop

	if err := f.Truncate(int64(sizeGiB) * 1024 * 1024 * 1024); err != nil {
		return fmt.Errorf("size vm disk %q to %dGiB: %w", path, sizeGiB, err)
	}

	if err := f.Close(); err != nil {
		return fmt.Errorf("close vm disk %q: %w", path, err)
	}

	return nil
}

// Wait waits for the cloud-hypervisor VMM process to exit.
func (m *vmManager) Wait() error {
	return m.cmd.Wait()
}

// startSwtpm launches a swtpm software TPM emulator bound to ctx. It listens on
// a control unix socket (tpmSocketPath) that cloud-hypervisor connects to for
// the guest's emulated TPM 2.0. Persistent TPM state lives under stateDir/tpm so
// it survives guest reboots within a run. The function returns once the control
// socket exists so a subsequent vm.create does not race swtpm startup.
func startSwtpm(ctx context.Context, stateDir string) error {
	tpmStateDir := filepath.Join(stateDir, "tpm")
	if err := os.MkdirAll(tpmStateDir, 0o755); err != nil {
		return fmt.Errorf("create tpm state dir: %w", err)
	}

	socket := tpmSocketPath()

	// Remove any stale socket so swtpm can bind cleanly on restart.
	if err := os.Remove(socket); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove stale tpm socket %q: %w", socket, err)
	}

	cmd := exec.CommandContext(ctx, swtpmBinary, "socket",
		"--tpmstate", "dir="+tpmStateDir,
		"--ctrl", "type=unixio,path="+socket,
		"--tpm2",
		"--flags", "startup-clear",
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start swtpm (%s): %w", swtpmBinary, err)
	}

	// Wait for swtpm to create its control socket before returning.
	deadline := time.Now().Add(10 * time.Second)

	for {
		if _, err := os.Stat(socket); err == nil {
			return nil
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("swtpm control socket %q not ready after 10s", socket)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// unixHTTPClient returns an HTTP client that dials the given unix socket for
// every request (cloud-hypervisor serves its REST API over a unix socket).
func unixHTTPClient(socket string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer

				return d.DialContext(ctx, "unix", socket)
			},
		},
	}
}

// waitReady blocks until the VMM answers a ping on its API socket or the
// timeout elapses.
func (m *vmManager) waitReady(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)

	for {
		_, status, err := m.do(ctx, http.MethodGet, "/api/v1/vmm.ping", nil)
		if err == nil && status == http.StatusOK {
			return nil
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("cloud-hypervisor api socket not ready after %s: %v", timeout, err)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// do issues one request to the VMM REST API. A non-nil body is JSON-encoded.
func (m *vmManager) do(ctx context.Context, method, path string, body any) ([]byte, int, error) {
	var reader io.Reader

	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, 0, fmt.Errorf("encode %s body: %w", path, err)
		}

		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, "http://localhost"+path, reader)
	if err != nil {
		return nil, 0, fmt.Errorf("build %s request: %w", path, err)
	}

	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := m.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close() //nolint:errcheck // response body close is best-effort

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("read %s response: %w", path, err)
	}

	return data, resp.StatusCode, nil
}

// info returns the guest's current VMM state. A guest that has not been created
// yet reports an empty state (the VMM answers non-2xx before vm.create).
func (m *vmManager) info(ctx context.Context) (chVMInfo, error) {
	data, status, err := m.do(ctx, http.MethodGet, "/api/v1/vm.info", nil)
	if err != nil {
		return chVMInfo{}, err
	}

	if status != http.StatusOK {
		// No guest created yet (or none running): treat as empty state.
		return chVMInfo{}, nil
	}

	var out chVMInfo
	if err := json.Unmarshal(data, &out); err != nil {
		return chVMInfo{}, fmt.Errorf("decode vm.info: %w", err)
	}

	return out, nil
}

// action issues a bodyless VMM action (vm.boot, vm.shutdown, ...), accepting any
// 2xx status.
func (m *vmManager) action(ctx context.Context, path string) error {
	data, status, err := m.do(ctx, http.MethodPut, path, nil)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}

	if status < 200 || status >= 300 {
		return fmt.Errorf("%s: unexpected status %d: %s", path, status, string(data))
	}

	return nil
}

// PowerState reports the guest power state as a Redfish PowerState value.
func (m *vmManager) PowerState(ctx context.Context) (string, error) {
	info, err := m.info(ctx)
	if err != nil {
		return "", err
	}

	if info.State == "Running" {
		return powerStateOn, nil
	}

	return powerStateOff, nil
}

// powerOn creates the guest if necessary and boots it. It is idempotent: a
// guest that is already running is left alone.
func (m *vmManager) powerOn(ctx context.Context) error {
	info, err := m.info(ctx)
	if err != nil {
		return err
	}

	switch info.State {
	case "Running":
		return nil
	case "Created", "Shutdown":
		return m.action(ctx, "/api/v1/vm.boot")
	default:
		// No guest yet: create from config then boot.
		if err := m.create(ctx); err != nil {
			return err
		}

		return m.action(ctx, "/api/v1/vm.boot")
	}
}

// powerOff shuts the guest down. It is idempotent: a guest that is not running
// is left alone.
func (m *vmManager) powerOff(ctx context.Context) error {
	info, err := m.info(ctx)
	if err != nil {
		return err
	}

	if info.State == "Running" || info.State == "Paused" {
		return m.action(ctx, "/api/v1/vm.shutdown")
	}

	return nil
}

// create sends the guest configuration to the VMM (vm.create).
func (m *vmManager) create(ctx context.Context) error {
	data, status, err := m.do(ctx, http.MethodPut, "/api/v1/vm.create", vmConfig(m.cfg, m.serialLog))
	if err != nil {
		return fmt.Errorf("vm.create: %w", err)
	}

	if status < 200 || status >= 300 {
		return fmt.Errorf("vm.create: unexpected status %d: %s", status, string(data))
	}

	return nil
}

// Reset applies a Redfish ComputerSystem.Reset action to the guest.
func (m *vmManager) Reset(ctx context.Context, rt ResetType) error {
	switch rt {
	case resetOn:
		return m.powerOn(ctx)
	case resetForceOff:
		return m.powerOff(ctx)
	case resetForceRestart:
		if err := m.powerOff(ctx); err != nil {
			return err
		}

		return m.powerOn(ctx)
	default:
		return fmt.Errorf("unsupported ResetType %q", rt)
	}
}
