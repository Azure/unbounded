// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package netboot

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/metalman/bootmedia"
)

const mediaFormat = "unbounded-uefi-v1"

// Version the ISO layout separately from the netboot image's EFI artifacts.
const mediaISOFormat = "unbounded-uefi-hybrid-v2"

var (
	mediaIDPattern     = regexp.MustCompile(`^[0-9a-f]{32}$`)
	pinnedImagePattern = regexp.MustCompile(`^.+@sha256:[0-9a-f]{64}$`)
	mediaTokenPattern  = regexp.MustCompile(`^[a-zA-Z0-9_.:/@+-]*$`)
)

// MediaProvider builds operation-owned hybrid ISOs (El Torito plus GPT ESP).
// Configure Resolver, Client (an uncached API client), CacheDir, and an advertised
// IPv4 TFTPServer.
// Share this instance through HTTPServer.Media and TFTPServer.Media. The
// controller must persist BootMedia and digest-pinned Netboot inputs first.
// Tokens are capabilities on the isolated management/provisioning network;
// TFTP has no source authentication. HTTP config and callbacks also require the
// first frozen lease's source IP, without trusting forwarding headers.
type MediaProvider struct {
	Resolver   *FileResolver
	Client     client.Client
	CacheDir   string
	TFTPServer string
	// Run optionally replaces the bounded command runner for tests.
	Run func(context.Context, string, ...string) error
	mu  sync.Mutex
}

var _ bootmedia.Provider = (*MediaProvider)(nil)

type mediaOwner struct {
	machine *v1alpha3.Machine
	op      *v1alpha3.MachineOperation
	index   int
}

func (o *mediaOwner) status() *v1alpha3.MachineOperationBootMediaStatus {
	return o.op.Status.Targets[o.index].BootMedia
}

// callbackOwner uses the frozen lease even after cleanup, failure, or Machine
// deletion. A running installer must still be able to cross the reboot barrier.
func (p *MediaProvider) callbackOwner(ctx context.Context, id string) (*mediaOwner, error) {
	if !mediaIDPattern.MatchString(id) || p.Client == nil {
		return nil, fmt.Errorf("invalid media capability or missing client")
	}

	var ops v1alpha3.MachineOperationList
	if err := p.Client.List(ctx, &ops); err != nil {
		return nil, err
	}

	var owner *mediaOwner

	for i := range ops.Items {
		op := &ops.Items[i]
		if op.Spec.OperationKind != v1alpha3.OperationHostReplace {
			continue
		}

		for j := range op.Status.Targets {
			t := &op.Status.Targets[j]
			if t.BootMedia == nil || t.BootMedia.ID != id {
				continue
			}

			if owner != nil {
				return nil, fmt.Errorf("duplicate media ownership")
			}

			if t.BootMedia.MachineUID == "" || t.BootMedia.Netboot == nil {
				return nil, fmt.Errorf("missing frozen media identity")
			}

			machine := v1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: t.MachineRef, UID: t.BootMedia.MachineUID}}
			machine.Spec.Host = &v1alpha3.HostSpec{Netboot: t.BootMedia.Netboot.DeepCopy()}
			machine.Status.Redfish = &v1alpha3.RedfishStatus{CertFingerprint: t.BootMedia.CertFingerprint}
			owner = &mediaOwner{machine: &machine, op: op, index: j}
		}
	}

	if owner == nil {
		return nil, fmt.Errorf("no active media owner")
	}

	return owner, nil
}

// owner authorizes new install work against live identity, but executes frozen
// inputs. Mutable Machine settings and certificate pins do not change this run.
func (p *MediaProvider) owner(ctx context.Context, id string) (*mediaOwner, error) {
	o, err := p.callbackOwner(ctx, id)
	if err != nil {
		return nil, err
	}

	target := o.op.Status.Targets[o.index]
	if o.op.Status.IsTerminal() || !o.op.DeletionTimestamp.IsZero() || target.Phase == v1alpha3.OperationPhaseComplete || target.Phase == v1alpha3.OperationPhaseFailed || o.status().Cleaned || o.status().ImageWritten {
		return nil, fmt.Errorf("media install authorization ended")
	}

	var current v1alpha3.Machine
	if err := p.Client.Get(ctx, client.ObjectKey{Name: target.MachineRef}, &current); err != nil {
		return nil, err
	}

	if current.UID != o.status().MachineUID || !current.DeletionTimestamp.IsZero() || (o.status().Site != "" && current.Labels["unbounded-cloud.io/site"] != o.status().Site) {
		return nil, fmt.Errorf("media machine identity or site ownership changed")
	}

	return o, nil
}

func (p *MediaProvider) Prepare(ctx context.Context, machine *v1alpha3.Machine, op *v1alpha3.MachineOperation, id string) (bootmedia.Source, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	o, err := p.owner(ctx, id)
	if err != nil {
		return bootmedia.Source{}, err
	}

	if machine == nil || op == nil || machine.UID != o.machine.UID || op.UID != o.op.UID || op.Name != o.op.Name || o.status().Cleaned || o.status().ImageWritten {
		return bootmedia.Source{}, fmt.Errorf("media is not owned by this active install")
	}

	if _, err := p.ensureISO(ctx, o); err != nil {
		return bootmedia.Source{}, err
	}

	return bootmedia.Source{Name: "ub-" + id + ".iso", Server: p.TFTPServer}, nil
}

// Release removes only local bytes. Durable Cleaned/terminal state revokes the
// capability and must be saved by the controller before calling Release.
func (p *MediaProvider) Release(_ context.Context, id string) error {
	if !mediaIDPattern.MatchString(id) || p.CacheDir == "" {
		return fmt.Errorf("invalid media ID or cache directory")
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	return os.RemoveAll(filepath.Join(p.CacheDir, "bootmedia", id))
}

// OpenISO authorizes a rack TFTP request before any host source-IP lookup.
func (p *MediaProvider) OpenISO(ctx context.Context, name string) (*os.File, error) {
	if !strings.HasPrefix(name, "ub-") || !strings.HasSuffix(name, ".iso") {
		return nil, fmt.Errorf("invalid media filename")
	}

	id := strings.TrimSuffix(strings.TrimPrefix(name, "ub-"), ".iso")

	p.mu.Lock()
	defer p.mu.Unlock()

	o, err := p.owner(ctx, id)
	if err != nil {
		return nil, err
	}

	if o.status().Cleaned || o.status().ImageWritten {
		return nil, fmt.Errorf("media install authorization ended")
	}

	path, err := p.ensureISO(ctx, o)
	if err != nil {
		return nil, err
	}

	return os.Open(path)
}

func mediaServeURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" || !mediaTokenPattern.MatchString(raw) || net.ParseIP(u.Hostname()).To4() == nil {
		return nil, fmt.Errorf("media serve URL must be plain HTTP with an IPv4 host and safe path")
	}

	if u.Path != "" && u.Path != "/" {
		return nil, fmt.Errorf("media serve URL must use the server root")
	}

	if u.Port() != "" {
		port, err := strconv.Atoi(u.Port())
		if err != nil || port < 1 || port > 65535 {
			return nil, fmt.Errorf("invalid media HTTP port")
		}
	}

	return u, nil
}

func validateMediaLease(spec *v1alpha3.PXESpec) error {
	if spec == nil || len(spec.DHCPLeases) == 0 {
		return fmt.Errorf("media requires a static lease")
	}

	l := spec.DHCPLeases[0]
	ip, gateway := net.ParseIP(l.IPv4), net.ParseIP(l.Gateway)
	prefix, err := subnetPrefix(l.SubnetMask)

	mac, macErr := net.ParseMAC(l.MAC)
	if ip.To4() == nil || gateway.To4() == nil || ip.String() != l.IPv4 || gateway.String() != l.Gateway || !ip.IsGlobalUnicast() || !gateway.IsGlobalUnicast() || err != nil || prefix < 1 || prefix > 30 || macErr != nil || len(mac) != 6 || l.MAC != mac.String() || mac[0]&1 != 0 {
		return fmt.Errorf("media requires an IPv4 lease, explicit subnet/gateway, and canonical Ethernet MAC")
	}

	if !(&net.IPNet{IP: ip.Mask(net.CIDRMask(prefix, 32)), Mask: net.CIDRMask(prefix, 32)}).Contains(gateway) {
		return fmt.Errorf("media gateway is outside the static subnet")
	}

	if !mediaTokenPattern.MatchString(spec.TargetDisk) {
		return fmt.Errorf("unsafe media target disk")
	}

	return nil
}

func (p *MediaProvider) ensureISO(ctx context.Context, o *mediaOwner) (string, error) {
	if p.Resolver == nil || p.Resolver.Cache == nil || p.CacheDir == "" || net.ParseIP(p.TFTPServer).To4() == nil || (o.status().SourceServer != p.TFTPServer && (o.status().SourceServer != "" || o.status().Phase != "Preparing")) {
		return "", fmt.Errorf("media resolver, cache, or advertised TFTP IPv4 is invalid or changed")
	}

	spec := o.status().Netboot
	if err := validateMediaLease(spec); err != nil {
		return "", err
	}

	if !pinnedImagePattern.MatchString(spec.Image) || !pinnedImagePattern.MatchString(spec.NetbootImage) {
		return "", fmt.Errorf("virtual media requires explicit digest-pinned disk and netboot images")
	}

	u, err := mediaServeURL(p.Resolver.ServeURL)
	if err != nil {
		return "", err
	}

	arch := spec.TargetArchitecture()
	boot, shim, grub := "BOOTX64.EFI", "shimx64.efi", "grubx64.efi"

	switch arch {
	case "amd64":
	case "arm64":
		boot, shim, grub = "BOOTAA64.EFI", "shimaa64.efi", "grubaa64.efi"
	default:
		return "", fmt.Errorf("unsupported media architecture %q", arch)
	}

	meta, err := p.Resolver.Cache.MetadataForRefArchitecture(spec.NetbootImage, arch)
	if err != nil {
		return "", err
	}

	if meta.MediaFormat != mediaFormat || meta.MediaShim != shim || meta.MediaGRUB != grub {
		return "", fmt.Errorf("netboot image lacks compatible %s media artifacts", mediaFormat)
	}
	// Validate the live installer template and its static values before any
	// BMC power action, but never copy the rendered install intent into the ISO.
	if _, err := p.installerConfig(o); err != nil {
		return "", err
	}

	config, err := bootstrapConfig(spec, u, o.status().ID)
	if err != nil {
		return "", err
	}
	// Only immutable inputs affect identity, not controller phase transitions.
	identity, err := json.Marshal([]any{mediaISOFormat, p.TFTPServer, p.Resolver.ServeURL, o.status().MachineUID, spec, o.status().ID})
	if err != nil {
		return "", err
	}

	hash := sha256.Sum256(identity)
	key := hex.EncodeToString(hash[:])
	dir := filepath.Join(p.CacheDir, "bootmedia", o.status().ID)

	iso := filepath.Join(dir, "bootstrap.iso")
	if old, err := os.ReadFile(filepath.Join(dir, "identity")); err == nil {
		if string(old) != key {
			return "", fmt.Errorf("immutable media inputs changed")
		}

		if _, err := os.Stat(iso); err == nil {
			return iso, nil
		}
	}

	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return "", err
	}

	stage, err := os.MkdirTemp(filepath.Dir(dir), ".build-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(stage) //nolint:errcheck // Best-effort removal of staging files.

	buildCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()

	if err := p.buildISO(buildCtx, stage, spec, boot, shim, grub, config); err != nil {
		return "", err
	}

	if err := os.WriteFile(filepath.Join(stage, "identity"), []byte(key), 0o600); err != nil {
		return "", err
	}

	if err := os.Rename(stage, dir); err != nil {
		return "", err
	}

	return iso, nil
}

func bootstrapConfig(spec *v1alpha3.PXESpec, u *url.URL, id string) ([]byte, error) {
	if err := validateMediaLease(spec); err != nil {
		return nil, err
	}

	if !mediaIDPattern.MatchString(id) {
		return nil, fmt.Errorf("invalid media ID")
	}

	l := spec.DHCPLeases[0]

	prefix, err := subnetPrefix(l.SubnetMask)
	if err != nil {
		return nil, err
	}

	var b strings.Builder
	b.WriteString("insmod efinet\ninsmod net\ninsmod http\nset ubnic=\"\"\n")
	// Firmware card numbering need not match Linux naming or lease order.
	for i := range 64 {
		// GRUB exports MAC variables for interfaces, not bare cards. A
		// temporary address probes each card without sending DHCP packets.
		fmt.Fprintf(&b, "if net_add_addr ubprobe efinet%d 0.0.0.0; then\n  if [ \"$net_ubprobe_mac\" = \"%s\" ]; then\n    set ubnic=efinet%d\n  fi\n  net_del_route ubprobe:local\n  net_del_addr ubprobe\nfi\n", i, l.MAC, i)
	}

	subnet := net.ParseIP(l.IPv4).Mask(net.CIDRMask(prefix, 32)).String()
	fmt.Fprintf(&b, "if [ -z \"$ubnic\" ]; then\n  halt\nfi\nnet_add_addr ubnet $ubnic %s\nnet_del_route ubnet:local\nnet_add_route ublocal %s/%d ubnet\nnet_add_route ubdefault 0.0.0.0/0 gw %s\nconfigfile (http,%s)/bootmedia/%s/grub.cfg\nhalt\n", l.IPv4, subnet, prefix, l.Gateway, u.Host, id)

	return []byte(b.String()), nil
}

func (p *MediaProvider) buildISO(ctx context.Context, dir string, spec *v1alpha3.PXESpec, boot, shim, grub string, config []byte) error {
	root := filepath.Join(dir, "root")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}

	fat := filepath.Join(dir, "efi.img")

	f, err := os.Create(fat)
	if err != nil {
		return err
	}

	if err := f.Truncate(8 << 20); err != nil {
		f.Close() //nolint:errcheck // Preserve the truncate error.
		return err
	}

	if err := f.Close(); err != nil {
		return err
	}

	run := p.Run
	if run == nil {
		run = runMediaCommand
	}

	if err := run(ctx, "mkfs.vfat", "--invariant", "-F", "16", "-s", "1", "-n", "UBMEDIA", fat); err != nil {
		return err
	}

	if err := run(ctx, "mmd", "-i", fat, "::/EFI", "::/EFI/BOOT", "::/EFI/ubuntu", "::/boot", "::/boot/grub", "::/grub"); err != nil {
		return err
	}

	for _, item := range []struct{ from, to string }{{shim, boot}, {grub, grub}} {
		src, tmpl, err := p.Resolver.Cache.ResolvePathForArchitecture(spec.NetbootImage, spec.TargetArchitecture(), item.from)
		if err != nil {
			return err
		}

		if tmpl {
			return fmt.Errorf("media EFI payload must not be a template")
		}

		if err := run(ctx, "mcopy", "-i", fat, src, "::/EFI/BOOT/"+item.to); err != nil {
			return err
		}
	}

	configPath := filepath.Join(dir, "grub.cfg")
	if err := os.WriteFile(configPath, config, 0o600); err != nil {
		return err
	}
	// Ubuntu GRUB builds use different compiled prefixes. All copies contain
	// only the same live config handoff, never install intent or credentials.
	// On Rack SCM USB media, GRUB sees (hd0,gpt2) and finds its config next
	// to the EFI binary.
	for _, dst := range []string{"/EFI/BOOT/grub.cfg", "/EFI/ubuntu/grub.cfg", "/boot/grub/grub.cfg", "/grub/grub.cfg", "/grub.cfg"} {
		if err := run(ctx, "mcopy", "-i", fat, configPath, "::"+dst); err != nil {
			return err
		}
	}

	if p.Run == nil {
		if err := normalizeMediaFAT(fat); err != nil {
			return err
		}
	}

	// Use the same FAT image for El Torito EFI boot and a GPT EFI System
	// Partition, since Rack SCM exposes the image as a removable USB disk.
	return run(ctx, "xorriso", "-as", "mkisofs", "-quiet", "-V", "UBMEDIA", "--modification-date=2020010100000000", "--set_all_file_dates", "2020010100000000", "-o", filepath.Join(dir, "bootstrap.iso"), "-append_partition", "2", "0xef", fat, "-appended_part_as_gpt", "-e", "--interval:appended_partition_2:all::", "-no-emul-boot", root)
}

// normalizeMediaFAT removes wall-clock directory timestamps, without touching
// file payloads. This small FAT16 image is generated locally, not user supplied.
func normalizeMediaFAT(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	if len(b) < 512 || string(b[54:62]) != "FAT16   " {
		return fmt.Errorf("unexpected media FAT format")
	}

	u16 := func(offset int) int { return int(binary.LittleEndian.Uint16(b[offset : offset+2])) }
	sector, cluster := u16(11), int(b[13])*u16(11)
	fatStart := u16(14) * sector
	root := fatStart + int(b[16])*u16(22)*sector
	rootSize := u16(17) * 32
	dataStart := root + ((rootSize+sector-1)/sector)*sector
	seen := map[int]bool{}

	var walk func(int, int) error

	walk = func(start, size int) error {
		if start < 0 || start+size > len(b) || size == 0 {
			return fmt.Errorf("invalid media FAT directory")
		}

		for pos := start; pos+32 <= start+size; pos += 32 {
			e := b[pos : pos+32]
			if e[0] == 0 {
				break
			}

			if e[0] == 0xe5 || e[11] == 0x0f {
				continue
			}

			clear(e[13:20])
			clear(e[22:26])
			// 2020-01-01, midnight, in the FAT packed date format.
			for _, off := range []int{16, 18, 24} {
				binary.LittleEndian.PutUint16(e[off:off+2], 0x5021)
			}

			if e[11]&0x10 == 0 || e[0] == '.' {
				continue
			}

			for c := int(binary.LittleEndian.Uint16(e[26:28])); c >= 2 && c < 0xfff8; {
				if seen[c] || fatStart+c*2+2 > len(b) {
					return fmt.Errorf("invalid media FAT cluster chain")
				}

				seen[c] = true
				if err := walk(dataStart+(c-2)*cluster, cluster); err != nil {
					return err
				}

				c = u16(fatStart + c*2)
			}
		}

		return nil
	}
	if err := walk(root, rootSize); err != nil {
		return err
	}

	return os.WriteFile(path, b, 0o600)
}

func runMediaCommand(ctx context.Context, name string, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, name, args...)

	cmd.Env = append(os.Environ(), "TZ=UTC", "SOURCE_DATE_EPOCH=1577836800")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
	cmd.WaitDelay = 10 * time.Second

	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("media command %s: %w", name, err)
	}

	return nil
}
