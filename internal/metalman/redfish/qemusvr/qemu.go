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
	"time"
)

// Machine is the QEMU-interaction layer. It drives a single libvirt domain
// through the virsh CLI and manages host networking and the HTTP boundary disk.
// It performs no Redfish semantics and holds no Redfish resource state; it shells
// out to external commands directly via os/exec and satisfies the Backend
// interface consumed by Server.
type Machine struct {
	domain          string
	bridge          string
	efiSource       string
	efiActive       string
	cacheDir        string
	manageBootOrder bool
}

// NewMachine builds the QEMU layer and, when an EFI source is configured, stages
// the blank boundary disk so it is present at the next power-on.
func NewMachine(cfg Config) (*Machine, error) {
	m := &Machine{
		domain:          cfg.Domain,
		bridge:          cfg.Bridge,
		efiSource:       cfg.EFISource,
		efiActive:       cfg.EFIActive,
		cacheDir:        cfg.CacheDir,
		manageBootOrder: cfg.ManageBootOrder,
	}

	if m.efiSource != "" {
		if err := m.StageEFIBoundary(false, "", ""); err != nil {
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

// DetachEFIBoundary removes the staged boundary disk after firmware has loaded.
// It sleeps first so OVMF has time to load shim/GRUB from the staged disk.
func (m *Machine) DetachEFIBoundary() {
	time.Sleep(60 * time.Second)

	//nolint:errcheck // Best-effort detach of the staged boundary disk.
	_, _, _ = m.virsh("detach-disk", m.domain, "vdb", "--live", "--config")
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

// withClientAddress attaches the static client IP to the HTTP boundary bridge,
// runs action, then removes the address.
func (m *Machine) withClientAddress(clientIP string, action func() error) error {
	if m.bridge == "" {
		return errors.New("UefiHttp requested without an HTTP boundary bridge")
	}

	if clientIP == "" {
		return errors.New("UefiHttp requested without a static client address")
	}

	if _, code, err := m.run("ip", "address", "add", clientIP+"/32", "dev", m.bridge); err != nil {
		return err
	} else if code != 0 {
		return fmt.Errorf("ip address add %s failed with code %d", clientIP, code)
	}

	defer func() {
		//nolint:errcheck // Best-effort removal of the temporary client address.
		_, _, _ = m.run("ip", "address", "delete", clientIP+"/32", "dev", m.bridge)
	}()

	return action()
}

// FetchBootEntrypoint emulates firmware's initial HTTP fetch after power-on by
// downloading bootURL over the boundary bridge from the static clientIP.
func (m *Machine) FetchBootEntrypoint(bootURL, clientIP string) error {
	return m.withClientAddress(clientIP, func() error {
		_, code, err := m.run("curl", "--fail", "--silent", "--show-error",
			"--interface", clientIP, "--output", "/dev/null", bootURL)
		if err != nil {
			return err
		}

		if code != 0 {
			return fmt.Errorf("curl %s failed with code %d", bootURL, code)
		}

		return nil
	})
}

// StageEFIBoundary makes the HTTP boundary disk visible at the next power-on.
// When enabled, it stages a FAT disk built from the cached HTTP entrypoint and
// artifacts fetched over the boundary bridge from clientIP. When disabled, it
// restores the blank EFI source.
func (m *Machine) StageEFIBoundary(enabled bool, bootURL, clientIP string) error {
	if m.efiSource == "" || m.efiActive == "" || m.bridge == "" || m.cacheDir == "" {
		return errors.New("UefiHttp requested without HTTP boundary arguments")
	}

	if !enabled {
		return copyFile(m.efiSource, m.efiActive)
	}

	if bootURL == "" {
		return errors.New("UefiHttp override has no HttpBootUri")
	}

	baseURL := bootURL[:strings.LastIndex(bootURL, "/")]
	entrypoint := bootURL[strings.LastIndex(bootURL, "/")+1:]

	return m.withClientAddress(clientIP, func() error {
		return m.stageBoundary(clientIP, baseURL, entrypoint)
	})
}

// stageBoundary downloads the boot artifacts and builds the boundary FAT image.
func (m *Machine) stageBoundary(clientIP, baseURL, entrypoint string) error {
	artifactDir, err := os.MkdirTemp("", "boundary-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(artifactDir) //nolint:errcheck // Best-effort cleanup of staging dir.

	candidates, err := filepath.Glob(filepath.Join(m.cacheDir, "oci", "*", "amd64", "disk", entrypoint))
	if err != nil {
		return err
	}

	if len(candidates) != 1 {
		return fmt.Errorf("expected one cached HTTP entrypoint %s, found %d", entrypoint, len(candidates))
	}

	if err := copyFile(candidates[0], filepath.Join(artifactDir, "http-entrypoint.efi")); err != nil {
		return err
	}

	downloads := []struct{ path, url string }{
		{"grubx64.efi", baseURL + "/grubx64.efi"},
		{"vmlinuz", baseURL + "/vmlinuz"},
		{"initrd", baseURL + "/initrd"},
		{"init.cpio", baseURL + "/init.cpio"},
		{"grub/grub.cfg", baseURL + "/grub/grub.cfg"},
	}
	for _, d := range downloads {
		target := filepath.Join(artifactDir, filepath.FromSlash(d.path))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}

		if _, code, err := m.run("curl", "--fail", "--silent", "--show-error",
			"--interface", clientIP, "--output", target, d.url); err != nil {
			return err
		} else if code != 0 {
			return fmt.Errorf("curl %s failed with code %d", d.url, code)
		}
	}

	boundary := filepath.Join(artifactDir, "boundary.img")
	if err := m.buildBoundaryImage(artifactDir, boundary); err != nil {
		return err
	}

	return copyFile(boundary, m.efiActive)
}

// buildBoundaryImage creates a FAT image sized to hold the staged artifacts and
// copies them into the expected layout.
func (m *Machine) buildBoundaryImage(artifactDir, boundary string) error {
	artifactSize, err := dirSize(artifactDir)
	if err != nil {
		return err
	}

	const (
		minSize   = 64 * 1024 * 1024
		slackBase = 32 * 1024 * 1024
	)

	slack := int64(slackBase)
	if artifactSize/4 > slack {
		slack = artifactSize / 4
	}

	boundarySize := int64(minSize)
	if artifactSize+slack > boundarySize {
		boundarySize = artifactSize + slack
	}

	if err := os.Truncate(boundary, boundarySize); err != nil {
		f, cerr := os.Create(boundary)
		if cerr != nil {
			return cerr
		}

		_ = f.Close() //nolint:errcheck // Best-effort close before truncating the created file.

		if err := os.Truncate(boundary, boundarySize); err != nil {
			return err
		}
	}

	if err := m.checkRun("mkfs.vfat", "-n", "HTTPBOOT", boundary); err != nil {
		return err
	}

	if err := m.checkRun("mmd", "-i", boundary, "::/EFI", "::/EFI/BOOT", "::/grub"); err != nil {
		return err
	}

	copies := []struct{ source, target string }{
		{"http-entrypoint.efi", "::/EFI/BOOT/BOOTX64.EFI"},
		{"grubx64.efi", "::/EFI/BOOT/grubx64.efi"},
		{"vmlinuz", "::/vmlinuz"},
		{"initrd", "::/initrd"},
		{"init.cpio", "::/init.cpio"},
		{"grub/grub.cfg", "::/grub/grub.cfg"},
		{"grub/grub.cfg", "::/EFI/BOOT/grub.cfg"},
	}
	for _, c := range copies {
		source := filepath.Join(artifactDir, filepath.FromSlash(c.source))
		if err := m.checkRun("mcopy", "-o", "-i", boundary, source, c.target); err != nil {
			return err
		}
	}

	return nil
}

// checkRun runs a command and returns an error on non-zero exit.
func (m *Machine) checkRun(name string, args ...string) error {
	_, code, err := m.run(name, args...)
	if err != nil {
		return err
	}

	if code != 0 {
		return fmt.Errorf("%s %s exited with code %d", name, strings.Join(args, " "), code)
	}

	return nil
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

func dirSize(root string) (int64, error) {
	var total int64

	err := filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if info.Mode().IsRegular() {
			total += info.Size()
		}

		return nil
	})

	return total, err
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close() //nolint:errcheck // Best-effort close of source file.

	out, err := os.Create(dst)
	if err != nil {
		return err
	}

	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close() //nolint:errcheck // Best-effort close on the error path.

		return err
	}

	return out.Close()
}
