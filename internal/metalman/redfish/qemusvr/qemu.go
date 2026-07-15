// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package qemusvr

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// pkOemPrefixGUID is gOvmfPkKek1AppPrefixGuid from OvmfPkg.dec. The custom
// CloudHv OVMF build ships an auto-dispatched EnrollDefaultKeys DXE driver that
// scans the SMBIOS type-11 OEM strings for an entry of the form
// "<pkOemPrefixGUID>:<base64 DER X509 cert>", enrolls that certificate as the
// Platform Key together with the compiled-in Microsoft KEK/db certificates, and
// so brings the guest up in Secure Boot user mode (SecureBoot=1, SetupMode=0) on
// every cold boot. Cloud Hypervisor delivers the string via
// --platform oem_strings=[...].
const pkOemPrefixGUID = "4e32566d-8e9e-4f52-81d3-5bb9715f9727"

// Machine is the Cloud Hypervisor interaction layer. It launches and controls a
// single cloud-hypervisor guest directly (no libvirt) and manages host
// networking. It performs no Redfish semantics and holds no Redfish resource
// state; it shells out to external commands directly via os/exec and satisfies
// the Backend interface consumed by Server.
//
// At construction it creates the boundary bridge, assigns the host IP, brings it
// up, and installs outbound NAT. Each power-on starts a swtpm software TPM,
// attaches a fresh tap to the bridge, and launches cloud-hypervisor. Power state
// tracks the cloud-hypervisor child; restarts issue a warm reboot over the
// ch-remote control API, mirroring the previous warm-reset semantics.
//
// Cloud Hypervisor loads UEFI firmware read-only from a single --firmware blob
// and has no separate NVRAM/pflash varstore, so there is no per-run NVRAM seed.
// Boot order is not expressed with per-device bootindex; the custom CloudHv OVMF
// falls back to network (PXE/HTTP) boot when the disk holds no bootable OS,
// which is exactly what the install path relies on.
//
// For UefiHttp overrides it translates the Redfish static-NIC and HttpBootUri
// settings into a dnsmasq configuration bound to the boundary bridge. The OVMF
// firmware then performs a genuine firmware-native UEFI HTTP boot: it DHCPs,
// learns its reserved address and the HTTPClient boot URL, and fetches the boot
// entrypoint over HTTP itself. No FAT boundary disk is built or staged.
type Machine struct {
	domain          string
	mac             string
	disk            string
	memoryMiB       int
	vcpus           int
	firmware        string
	secureBoot      bool
	oemStringPK     string // SMBIOS PK OEM string, set only when secureBoot
	manageBootOrder bool

	bridge       string
	bridgeAddr   string
	bridgePrefix int
	subnet       string // derived network CIDR, e.g. 192.168.200.0/24
	tap          string

	stateDir   string
	apiSock    string
	serialSock string
	tpmStateD  string
	tpmSock    string

	dnsmasqDir string

	mu          sync.Mutex
	ch          *exec.Cmd
	swtpm       *exec.Cmd
	dnsmasq     *exec.Cmd
	bootNetwork bool // recorded boot preference; CloudHv OVMF falls back to net
}

// NewMachine builds the Cloud Hypervisor layer, deriving working paths from the
// state directory and creating the boundary bridge and NAT. When Secure Boot is
// requested it generates an ephemeral Platform Key and encodes it as the SMBIOS
// OEM string the firmware enrolls at boot.
func NewMachine(cfg Config) (*Machine, error) {
	if cfg.StateDir == "" {
		return nil, errors.New("qemusvr: --state-dir is required")
	}

	if cfg.Disk == "" {
		return nil, errors.New("qemusvr: --disk is required")
	}

	firmware := cfg.Firmware
	if cfg.SecureBoot {
		firmware = cfg.FirmwareSecureBoot
	}

	if firmware == "" {
		return nil, errors.New("qemusvr: --firmware (and --firmware-secureboot when --secure-boot is set) is required")
	}

	memoryMiB := cfg.MemoryMiB
	if memoryMiB == 0 {
		memoryMiB = 4096
	}

	vcpus := cfg.VCPUs
	if vcpus == 0 {
		vcpus = 2
	}

	prefix := cfg.BridgePrefix
	if prefix == 0 {
		prefix = 24
	}

	m := &Machine{
		domain:          cfg.Domain,
		mac:             strings.ToLower(cfg.MAC),
		disk:            cfg.Disk,
		memoryMiB:       memoryMiB,
		vcpus:           vcpus,
		firmware:        firmware,
		secureBoot:      cfg.SecureBoot,
		manageBootOrder: cfg.ManageBootOrder,
		bridge:          cfg.Bridge,
		bridgeAddr:      cfg.BridgeAddress,
		bridgePrefix:    prefix,
		stateDir:        cfg.StateDir,
		apiSock:         filepath.Join(cfg.StateDir, "api.sock"),
		serialSock:      filepath.Join(cfg.StateDir, "console.sock"),
		tpmStateD:       filepath.Join(cfg.StateDir, "tpm"),
		tpmSock:         filepath.Join(cfg.StateDir, "tpm", "swtpm.sock"),
		dnsmasqDir:      cfg.DnsmasqDir,
		tap:             tapName(cfg.Bridge),
	}

	if m.secureBoot {
		oemString, err := platformKeyOEMString()
		if err != nil {
			return nil, fmt.Errorf("generating Secure Boot platform key: %w", err)
		}

		m.oemStringPK = oemString
	}

	if err := os.MkdirAll(m.stateDir, 0o755); err != nil {
		return nil, err
	}

	if err := os.MkdirAll(m.tpmStateD, 0o755); err != nil {
		return nil, err
	}

	if m.dnsmasqDir != "" {
		if err := os.MkdirAll(m.dnsmasqDir, 0o755); err != nil {
			return nil, err
		}
	}

	if m.bridge != "" {
		subnet, err := subnetCIDR(m.bridgeAddr, m.bridgePrefix)
		if err != nil {
			return nil, err
		}

		m.subnet = subnet

		if err := m.setupNetwork(); err != nil {
			return nil, err
		}
	}

	return m, nil
}

// platformKeyOEMString generates an ephemeral self-signed X509 certificate and
// encodes it as the "<pkOemPrefixGUID>:<base64 DER>" SMBIOS OEM string that the
// CloudHv OVMF EnrollDefaultKeys driver consumes as the Secure Boot Platform Key.
func platformKeyOEMString() (string, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return "", err
	}

	template := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "metalman-fixture-PK"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return "", err
	}

	return pkOemPrefixGUID + ":" + base64.StdEncoding.EncodeToString(der), nil
}

// tapName derives a stable tap device name from the bridge name, keeping it
// within the kernel's 15-character interface-name limit.
func tapName(bridge string) string {
	name := "tap-" + strings.TrimPrefix(bridge, "virbr-")
	if len(name) > 15 {
		name = name[:15]
	}

	return name
}

// subnetCIDR returns the network CIDR (e.g. 192.168.200.0/24) that contains addr
// with the given prefix length.
func subnetCIDR(addr string, prefix int) (string, error) {
	ip := net.ParseIP(addr)
	if ip == nil {
		return "", fmt.Errorf("invalid bridge address %q", addr)
	}

	mask := net.CIDRMask(prefix, 32)
	network := ip.Mask(mask)

	return fmt.Sprintf("%s/%d", network.String(), prefix), nil
}

// run executes name with args, returning combined output for diagnostics.
func run(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)

	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, out)
	}

	return string(out), nil
}

// setupNetwork creates the boundary bridge, assigns the host address, brings it
// up, and installs outbound NAT and forwarding for the guest subnet. It is
// idempotent so a stale bridge from a previous run does not fail startup.
func (m *Machine) setupNetwork() error {
	if _, err := run("ip", "link", "show", m.bridge); err != nil {
		if _, err := run("ip", "link", "add", m.bridge, "type", "bridge"); err != nil {
			return err
		}
	}

	//nolint:errcheck // The address may already be present from a prior run.
	_, _ = run("ip", "addr", "add", fmt.Sprintf("%s/%d", m.bridgeAddr, m.bridgePrefix), "dev", m.bridge)

	if _, err := run("ip", "link", "set", m.bridge, "up"); err != nil {
		return err
	}

	if err := os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1\n"), 0o644); err != nil {
		return fmt.Errorf("enabling ip_forward: %w", err)
	}

	// Install NAT and forwarding, checking first so re-runs stay idempotent.
	m.ensureIPTables("nat", "POSTROUTING", "-s", m.subnet, "!", "-d", m.subnet, "-j", "MASQUERADE")
	m.ensureIPTables("filter", "FORWARD", "-i", m.bridge, "-j", "ACCEPT")
	m.ensureIPTables("filter", "FORWARD", "-o", m.bridge, "-j", "ACCEPT")

	return nil
}

// ensureIPTables appends an iptables rule to the given table/chain only when it
// is not already present. It is best-effort; failures are surfaced to stderr.
func (m *Machine) ensureIPTables(table, chain string, rule ...string) {
	check := append([]string{"-t", table, "-C", chain}, rule...)
	if _, err := run("iptables", check...); err == nil {
		return
	}

	add := append([]string{"-t", table, "-A", chain}, rule...)
	if out, err := run("iptables", add...); err != nil {
		fmt.Fprintf(os.Stderr, "qemusvr: iptables %s: %v (%s)\n", strings.Join(add, " "), err, out)
	}
}

// teardownNetwork removes the NAT/forwarding rules and the bridge on shutdown.
func (m *Machine) teardownNetwork() {
	if m.bridge == "" {
		return
	}

	//nolint:errcheck // Best-effort teardown of the boundary networking.
	_, _ = run("iptables", "-t", "nat", "-D", "POSTROUTING", "-s", m.subnet, "!", "-d", m.subnet, "-j", "MASQUERADE")
	//nolint:errcheck // Best-effort teardown of the boundary networking.
	_, _ = run("iptables", "-t", "filter", "-D", "FORWARD", "-i", m.bridge, "-j", "ACCEPT")
	//nolint:errcheck // Best-effort teardown of the boundary networking.
	_, _ = run("iptables", "-t", "filter", "-D", "FORWARD", "-o", m.bridge, "-j", "ACCEPT")
	//nolint:errcheck // Best-effort teardown of the boundary bridge.
	_, _ = run("ip", "link", "delete", m.bridge)
}

// PowerState returns "On" when the cloud-hypervisor child is running, otherwise
// "Off".
func (m *Machine) PowerState() string {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.chRunningLocked() {
		return "On"
	}

	return "Off"
}

// chRunningLocked reports whether the cloud-hypervisor child is still alive. The
// caller holds m.mu.
func (m *Machine) chRunningLocked() bool {
	if m.ch == nil || m.ch.Process == nil {
		return false
	}

	// Signal 0 probes liveness without affecting the process. A nil os.Signal
	// is rejected by the runtime as an unsupported signal type, so the raw
	// syscall.Signal(0) must be used to perform a genuine kill(pid, 0) probe.
	return m.ch.Process.Signal(syscall.Signal(0)) == nil
}

// PowerOff forces the guest off. It is best-effort and never returns an error.
func (m *Machine) PowerOff() {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.stopGuestLocked()
}

// PowerOn launches the guest with the current boot order. It is a no-op if the
// guest is already running.
func (m *Machine) PowerOn() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.chRunningLocked() {
		return nil
	}

	return m.startGuestLocked()
}

// Restart issues a warm reboot over the ch-remote control API on a running
// guest, or cold-starts a stopped one.
func (m *Machine) Restart() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.chRunningLocked() {
		return m.startGuestLocked()
	}

	if _, err := run("ch-remote", "--api-socket", m.apiSock, "reboot"); err != nil {
		return fmt.Errorf("ch-remote reboot: %w", err)
	}

	return nil
}

// SetBootOrder records the boot order for the next power-on when boot-order
// management is enabled. Cloud Hypervisor has no per-device boot index, so this
// is a recorded preference only: the CloudHv OVMF firmware boots the disk when
// it holds a bootable OS and otherwise falls back to network boot, which matches
// what "Pxe" requests during install.
func (m *Machine) SetBootOrder(target string) error {
	if !m.manageBootOrder {
		return nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	m.bootNetwork = target == "Pxe"

	return nil
}

// startGuestLocked starts swtpm, attaches a fresh tap to the bridge, and
// launches cloud-hypervisor. The caller holds m.mu.
func (m *Machine) startGuestLocked() error {
	if err := m.startSwtpmLocked(); err != nil {
		return err
	}

	if err := m.ensureTap(); err != nil {
		m.stopSwtpmLocked()
		return err
	}

	//nolint:errcheck // Stale sockets from a prior run must not block bind (AddrInUse).
	_ = os.Remove(m.apiSock)
	//nolint:errcheck // The serial socket is recreated on each boot; a leftover file causes AddrInUse.
	_ = os.Remove(m.serialSock)

	spec := chSpec{
		memoryMiB:   m.memoryMiB,
		vcpus:       m.vcpus,
		disk:        m.disk,
		firmware:    m.firmware,
		mac:         m.mac,
		tap:         m.tap,
		apiSock:     m.apiSock,
		serialSock:  m.serialSock,
		tpmSock:     m.tpmSock,
		oemStringPK: m.oemStringPK,
	}

	cmd := exec.Command("cloud-hypervisor", chArgs(spec)...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		m.stopSwtpmLocked()
		m.deleteTap()

		return fmt.Errorf("starting cloud-hypervisor: %w", err)
	}

	m.ch = cmd

	return nil
}

// stopGuestLocked terminates and reaps cloud-hypervisor, then tears down swtpm
// and the tap. The caller holds m.mu.
func (m *Machine) stopGuestLocked() {
	if m.ch != nil && m.ch.Process != nil {
		//nolint:errcheck // Best-effort forced power off.
		_ = m.ch.Process.Kill()
		//nolint:errcheck // Reap the killed process; the wait error is expected.
		_ = m.ch.Wait()
		m.ch = nil
	}

	m.stopSwtpmLocked()
	m.deleteTap()
}

// startSwtpmLocked launches a fresh swtpm software TPM listening on a unix
// control socket for cloud-hypervisor's --tpm device. The caller holds m.mu.
func (m *Machine) startSwtpmLocked() error {
	m.stopSwtpmLocked()

	//nolint:errcheck // A stale socket from a prior run must not block bind.
	_ = os.Remove(m.tpmSock)

	cmd := exec.Command("swtpm", "socket",
		"--tpmstate", "dir="+m.tpmStateD,
		"--ctrl", "type=unixio,path="+m.tpmSock,
		"--tpm2",
	)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting swtpm: %w", err)
	}

	m.swtpm = cmd

	// Wait briefly for the control socket so cloud-hypervisor connects cleanly.
	for range 50 {
		if _, err := os.Stat(m.tpmSock); err == nil {
			return nil
		}

		time.Sleep(20 * time.Millisecond)
	}

	return nil
}

// stopSwtpmLocked terminates and reaps swtpm. The caller holds m.mu.
func (m *Machine) stopSwtpmLocked() {
	if m.swtpm == nil || m.swtpm.Process == nil {
		return
	}

	//nolint:errcheck // Best-effort teardown of the software TPM.
	_ = m.swtpm.Process.Kill()
	//nolint:errcheck // Reap the killed process; the wait error is expected.
	_ = m.swtpm.Wait()
	m.swtpm = nil
}

// ensureTap creates the tap device (if absent), enslaves it to the bridge, and
// brings it up so cloud-hypervisor can open it by name.
func (m *Machine) ensureTap() error {
	if m.bridge == "" {
		return errors.New("qemusvr: a boundary bridge is required to attach the guest NIC")
	}

	if _, err := run("ip", "link", "show", m.tap); err != nil {
		if _, err := run("ip", "tuntap", "add", "dev", m.tap, "mode", "tap"); err != nil {
			return err
		}
	}

	if _, err := run("ip", "link", "set", m.tap, "master", m.bridge); err != nil {
		return err
	}

	if _, err := run("ip", "link", "set", m.tap, "up"); err != nil {
		return err
	}

	return nil
}

// deleteTap removes the guest tap device. It is best-effort.
func (m *Machine) deleteTap() {
	if m.tap == "" {
		return
	}

	//nolint:errcheck // Best-effort teardown of the guest tap.
	_, _ = run("ip", "link", "delete", m.tap)
}

// Shutdown tears down the guest, software TPM, dnsmasq, tap, and bridge. It is
// called once on process exit.
func (m *Machine) Shutdown() {
	m.mu.Lock()
	m.stopGuestLocked()
	m.stopDnsmasqLocked()
	m.mu.Unlock()

	m.teardownNetwork()
}

// chSpec is the pure input to chArgs, capturing everything needed to build the
// cloud-hypervisor command line.
type chSpec struct {
	memoryMiB   int
	vcpus       int
	disk        string
	firmware    string
	mac         string
	tap         string
	apiSock     string
	serialSock  string
	tpmSock     string
	oemStringPK string // when set, delivered via --platform oem_strings
}

// chArgs builds the cloud-hypervisor argument vector for spec. It is pure so it
// can be unit tested without launching a guest.
//
// cloud-hypervisor exposes exactly one socket-backed character device: the
// legacy serial port (--serial socket=). The virtio console (--console) only
// supports off|null|pty|tty|file, so it cannot carry a socket. We therefore
// bind ttyS0 to serialSock and disable the virtio console. serialSock carries
// the kernel console, the autologin getty shell, and the automation channel the
// smoke tests drive; the marker-based command protocol tolerates interleaved
// kernel output. When oemStringPK is set it is delivered as an SMBIOS type-11
// OEM string so the firmware enrolls the Secure Boot Platform Key.
func chArgs(spec chSpec) []string {
	args := []string{
		"--api-socket", spec.apiSock,
		"--cpus", "boot=" + strconv.Itoa(spec.vcpus),
		"--memory", fmt.Sprintf("size=%dM", spec.memoryMiB),
		"--firmware", spec.firmware,
		"--disk", "path=" + spec.disk,
		"--net", fmt.Sprintf("tap=%s,mac=%s", spec.tap, spec.mac),
		"--tpm", "socket=" + spec.tpmSock,
		"--serial", "socket=" + spec.serialSock,
		"--console", "off",
		"--rng", "src=/dev/urandom",
	}

	if spec.oemStringPK != "" {
		args = append(args, "--platform", "oem_strings=["+spec.oemStringPK+"]")
	}

	return args
}

// ConfigureHTTPBoot writes a dnsmasq configuration that hands the VM its static
// address plus a UEFI HTTP boot URL via a DHCP reservation, then (re)starts
// dnsmasq bound to the boundary bridge. The CloudHv OVMF firmware then performs
// a genuine firmware-native HTTP boot from the URL.
func (m *Machine) ConfigureHTTPBoot(mac, address, subnetMask, gateway string, dns []string, bootURL string) error {
	if m.bridge == "" || m.dnsmasqDir == "" {
		return errors.New("UefiHttp requested without a boundary bridge and dnsmasq directory")
	}

	if mac == "" || address == "" || subnetMask == "" || bootURL == "" {
		return errors.New("UefiHttp requires a MAC, static address, subnet mask, and boot URL")
	}

	conf := m.renderDnsmasqConf(mac, address, subnetMask, gateway, dns, bootURL)

	confPath := filepath.Join(m.dnsmasqDir, "dnsmasq.conf")
	if err := os.WriteFile(confPath, []byte(conf), 0o644); err != nil {
		return err
	}

	return m.restartDnsmasq(confPath)
}

// renderDnsmasqConf builds the dnsmasq configuration that reserves the VM's
// address and advertises the UEFI HTTP boot URL. DNS is disabled (port=0); it
// owns only DHCP on the boundary bridge.
func (m *Machine) renderDnsmasqConf(mac, address, subnetMask, gateway string, dns []string, bootURL string) string {
	var b strings.Builder

	fmt.Fprintln(&b, "port=0")
	fmt.Fprintf(&b, "interface=%s\n", m.bridge)
	fmt.Fprintln(&b, "bind-interfaces")
	fmt.Fprintln(&b, "dhcp-authoritative")
	fmt.Fprintf(&b, "dhcp-leasefile=%s\n", filepath.Join(m.dnsmasqDir, "dnsmasq.leases"))
	fmt.Fprintf(&b, "log-facility=%s\n", filepath.Join(m.dnsmasqDir, "dnsmasq.log"))
	fmt.Fprintf(&b, "dhcp-range=%s,%s,%s,infinite\n", address, address, subnetMask)
	fmt.Fprintf(&b, "dhcp-host=%s,%s,infinite\n", mac, address)

	if gateway != "" {
		fmt.Fprintf(&b, "dhcp-option=3,%s\n", gateway)
	}

	if len(dns) > 0 {
		fmt.Fprintf(&b, "dhcp-option=6,%s\n", strings.Join(dns, ","))
	}

	// x64 UEFI HTTP clients advertise architecture 16 (RFC 3925/UEFI). Answer
	// them with the HTTPClient vendor class and the boot URL as the bootfile so
	// OVMF's HttpBootDxe fetches it directly.
	fmt.Fprintln(&b, "dhcp-match=set:httpboot,option:client-arch,16")
	fmt.Fprintf(&b, "dhcp-boot=tag:httpboot,%s\n", bootURL)
	fmt.Fprintln(&b, "dhcp-option-force=tag:httpboot,60,HTTPClient")

	return b.String()
}

// restartDnsmasq stops any running dnsmasq and starts a fresh one from confPath.
func (m *Machine) restartDnsmasq(confPath string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.stopDnsmasqLocked()

	cmd := exec.Command("dnsmasq", "--keep-in-foreground", "--conf-file="+confPath)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return err
	}

	m.dnsmasq = cmd

	return nil
}

// ClearHTTPBoot stops the boundary dnsmasq so no DHCP reservation is served once
// the HTTP boot override is disabled.
func (m *Machine) ClearHTTPBoot() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.stopDnsmasqLocked()

	return nil
}

// stopDnsmasqLocked terminates and reaps the running dnsmasq. The caller holds
// m.mu.
func (m *Machine) stopDnsmasqLocked() {
	if m.dnsmasq == nil || m.dnsmasq.Process == nil {
		return
	}

	//nolint:errcheck // Best-effort teardown of the boundary dnsmasq.
	_ = m.dnsmasq.Process.Kill()
	//nolint:errcheck // Reap the killed process; the wait error is expected.
	_ = m.dnsmasq.Wait()

	m.dnsmasq = nil
}
