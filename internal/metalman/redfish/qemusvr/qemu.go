// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package qemusvr

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// Machine is the QEMU-interaction layer. It drives a single libvirt domain
// through the virsh CLI and manages host networking. It performs no Redfish
// semantics and holds no Redfish resource state; it shells out to external
// commands directly via os/exec and satisfies the Backend interface consumed by
// Server.
//
// For UefiHttp overrides it translates the Redfish static-NIC and HttpBootUri
// settings into a dnsmasq configuration bound to the boundary bridge. Stock OVMF
// then performs a genuine firmware-native UEFI HTTP boot: it DHCPs, learns its
// reserved address and the HTTPClient boot URL, and fetches the boot entrypoint
// over HTTP itself. No FAT boundary disk is built or staged.
type Machine struct {
	domain          string
	bridge          string
	mac             string
	dnsmasqDir      string
	manageBootOrder bool

	mu      sync.Mutex
	dnsmasq *exec.Cmd
}

// NewMachine builds the QEMU layer and ensures the dnsmasq working directory
// exists when HTTP boot support is configured.
func NewMachine(cfg Config) (*Machine, error) {
	m := &Machine{
		domain:          cfg.Domain,
		bridge:          cfg.Bridge,
		mac:             strings.ToLower(cfg.MAC),
		dnsmasqDir:      cfg.DnsmasqDir,
		manageBootOrder: cfg.ManageBootOrder,
	}

	if m.dnsmasqDir != "" {
		if err := os.MkdirAll(m.dnsmasqDir, 0o755); err != nil {
			return nil, err
		}
	}

	return m, nil
}

// run executes name with args and returns stdout and the process exit code. err
// is non-nil only when the command could not be started or run to completion; a
// non-zero exit is reported through the exit code, not an error.
func (m *Machine) run(name string, args ...string) (string, int, error) {
	stdout, err := exec.Command(name, args...).Output()

	var exit *exec.ExitError
	if err != nil {
		if errors.As(err, &exit) {
			return string(stdout), exit.ExitCode(), nil
		}

		return string(stdout), -1, err
	}

	return string(stdout), 0, nil
}

// virsh runs a virsh subcommand against the system libvirt instance.
func (m *Machine) virsh(args ...string) (string, int, error) {
	return m.run("virsh", append([]string{"--connect", "qemu:///system"}, args...)...)
}

// virshCheck runs virsh and returns an error on non-zero exit.
func (m *Machine) virshCheck(args ...string) (string, error) {
	stdout, code, err := m.virsh(args...)
	if err != nil {
		return stdout, err
	}

	if code != 0 {
		return stdout, fmt.Errorf("virsh %s exited with code %d", strings.Join(args, " "), code)
	}

	return stdout, nil
}

// PowerState returns "On" when the domain is running, otherwise "Off".
func (m *Machine) PowerState() string {
	stdout, code, err := m.virsh("domstate", m.domain)
	if err == nil && code == 0 && strings.Contains(stdout, "running") {
		return "On"
	}

	return "Off"
}

// PowerOff forces the domain off. It is best-effort and never returns an error.
func (m *Machine) PowerOff() {
	//nolint:errcheck // Best-effort power off.
	_, _, _ = m.virsh("destroy", m.domain)
}

// PowerOn starts the domain.
func (m *Machine) PowerOn() error {
	_, err := m.virshCheck("start", m.domain)

	return err
}

// Restart resets a running domain or starts a stopped one.
func (m *Machine) Restart() error {
	if m.PowerState() == "On" {
		_, err := m.virshCheck("reset", m.domain)

		return err
	}

	_, err := m.virshCheck("start", m.domain)

	return err
}

// SetBootOrder rewrites the libvirt domain boot order when boot-order management
// is enabled. target "Pxe" boots the network first, anything else boots disk
// first.
func (m *Machine) SetBootOrder(target string) error {
	if !m.manageBootOrder {
		return nil
	}

	current, err := m.virshCheck("dumpxml", m.domain)
	if err != nil {
		return err
	}

	devices := []string{"hd", "network"}
	if target == "Pxe" {
		devices = []string{"network", "hd"}
	}

	rewritten, err := rewriteBootOrder(current, devices)
	if err != nil {
		return err
	}

	tmp, err := os.CreateTemp("", "domain-*.xml")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) //nolint:errcheck // Best-effort cleanup of temp file.

	if _, err := tmp.WriteString(rewritten); err != nil {
		_ = tmp.Close() //nolint:errcheck // Best-effort close on the error path.

		return err
	}

	if err := tmp.Close(); err != nil {
		return err
	}

	_, err = m.virshCheck("define", tmp.Name())

	return err
}

// ConfigureHTTPBoot writes a dnsmasq configuration that hands the VM its static
// address plus a UEFI HTTP boot URL via a DHCP reservation, then (re)starts
// dnsmasq bound to the boundary bridge. Stock OVMF then performs a genuine
// firmware-native HTTP boot from the URL.
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
// address and advertises the UEFI HTTP boot URL. DNS is disabled (port=0) so it
// does not collide with libvirt's per-network dnsmasq; it owns only DHCP on the
// boundary bridge.
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

// rewriteBootOrder replaces the <boot> children of the <os> element with one
// entry per device, preserving the rest of the domain XML.
func rewriteBootOrder(domainXML string, devices []string) (string, error) {
	dec := xml.NewDecoder(strings.NewReader(domainXML))

	var out strings.Builder

	enc := xml.NewEncoder(&out)
	inOS := false

	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			return "", err
		}

		switch t := tok.(type) {
		case xml.StartElement:
			if inOS && t.Name.Local == "boot" {
				if err := dec.Skip(); err != nil {
					return "", err
				}

				continue
			}

			if t.Name.Local == "os" {
				inOS = true
			}
		case xml.EndElement:
			if inOS && t.Name.Local == "os" {
				for _, dev := range devices {
					start := xml.StartElement{
						Name: xml.Name{Local: "boot"},
						Attr: []xml.Attr{{Name: xml.Name{Local: "dev"}, Value: dev}},
					}
					if err := enc.EncodeToken(start); err != nil {
						return "", err
					}

					if err := enc.EncodeToken(xml.EndElement{Name: start.Name}); err != nil {
						return "", err
					}
				}

				inOS = false
			}
		}

		if err := enc.EncodeToken(tok); err != nil {
			return "", err
		}
	}

	if err := enc.Flush(); err != nil {
		return "", err
	}

	return out.String(), nil
}
