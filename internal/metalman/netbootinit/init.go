// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package netbootinit

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	defaultSysfsRoot     = "/sys"
	defaultProcCmdline   = "/proc/cmdline"
	defaultMountRoot     = "/mnt"
	defaultESPMountPoint = "/mnt/esp"
	logPrefix            = "metalman"
)

// Installer implements the metalman netboot init process. It is written so the
// hardware-facing pieces are small wrappers around testable decision logic.
type Installer struct {
	SysfsRoot     string
	ProcCmdline   string
	MountRoot     string
	ESPMountPoint string
	Logger        *Logger
	Runner        CommandRunner
	HTTPClient    *http.Client
	Sleep         func(time.Duration)
}

// CommandRunner runs external programs that are already present in the Ubuntu
// netboot initrd, such as modprobe, mount, ip, blockdev, and efibootmgr.
type CommandRunner interface {
	Run(ctx context.Context, name string, args ...string) error
	Output(ctx context.Context, name string, args ...string) (string, error)
	LookPath(name string) (string, error)
}

type realCommandRunner struct{}

func (realCommandRunner) Run(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)

	out, err := cmd.CombinedOutput()
	if err != nil {
		return formatCommandError(name, args, err, out)
	}

	return nil
}

func (realCommandRunner) Output(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)

	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", formatCommandError(name, args, err, out)
	}

	return strings.TrimSpace(string(out)), nil
}

func (realCommandRunner) LookPath(name string) (string, error) { return exec.LookPath(name) }

func formatCommandError(name string, args []string, err error, out []byte) error {
	msg := strings.TrimSpace(string(out))
	if msg == "" {
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}

	return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, msg)
}

func runBestEffort(ctx context.Context, runner CommandRunner, name string, args ...string) {
	if err := runner.Run(ctx, name, args...); err != nil {
		return
	}
}

func closeBestEffort(c io.Closer) {
	if err := c.Close(); err != nil {
		return
	}
}

// Logger writes init messages with the same prefix as the old shell script.
type Logger struct {
	mu  sync.Mutex
	out io.Writer
}

func NewLogger(out io.Writer) *Logger {
	return &Logger{out: out}
}

func NewKernelLogger() *Logger {
	f, err := os.OpenFile("/dev/kmsg", os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return NewLogger(os.Stderr)
	}

	return NewLogger(f)
}

func (l *Logger) Printf(format string, args ...any) {
	if l == nil {
		return
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	fmt.Fprintf(l.out, "%s: %s\n", logPrefix, fmt.Sprintf(format, args...)) //nolint:errcheck // Best-effort initrd logging.
}

// NewInstaller returns an installer configured for the real initrd environment.
func NewInstaller() *Installer {
	return &Installer{
		SysfsRoot:     defaultSysfsRoot,
		ProcCmdline:   defaultProcCmdline,
		MountRoot:     defaultMountRoot,
		ESPMountPoint: defaultESPMountPoint,
		Runner:        realCommandRunner{},
		HTTPClient:    http.DefaultClient,
		Sleep:         time.Sleep,
	}
}

func (i *Installer) normalize() {
	if i.SysfsRoot == "" {
		i.SysfsRoot = defaultSysfsRoot
	}

	if i.ProcCmdline == "" {
		i.ProcCmdline = defaultProcCmdline
	}

	if i.MountRoot == "" {
		i.MountRoot = defaultMountRoot
	}

	if i.ESPMountPoint == "" {
		i.ESPMountPoint = defaultESPMountPoint
	}

	if i.Runner == nil {
		i.Runner = realCommandRunner{}
	}

	if i.HTTPClient == nil {
		i.HTTPClient = http.DefaultClient
	}

	if i.Sleep == nil {
		i.Sleep = time.Sleep
	}
}

// Run performs the full netboot install flow.
func (i *Installer) Run(ctx context.Context) error {
	i.normalize()

	if err := i.setupMounts(); err != nil {
		return err
	}

	if i.Logger == nil {
		i.Logger = NewKernelLogger()
	}

	if err := os.Setenv("PATH", "/usr/sbin:/usr/bin:/sbin:/bin"); err != nil {
		return fmt.Errorf("setting PATH: %w", err)
	}

	i.Logger.Printf("installer starting")

	if err := i.loadKernelModules(ctx); err != nil {
		return err
	}

	cmdlineBytes, err := os.ReadFile(i.ProcCmdline)
	if err != nil {
		return fmt.Errorf("reading kernel command line: %w", err)
	}

	params := parseCmdline(string(cmdlineBytes))

	imageURL := params["unbounded.image_url"]
	if imageURL == "" {
		return errors.New("unbounded.image_url not set")
	}

	serveURL := params["unbounded.serve_url"]
	targetDisk := params["unbounded.disk"]

	bootMAC := normalizeMAC(params["unbounded.boot_mac"])
	if bootMAC == "" && params["BOOTIF"] != "" {
		bootMAC = bootifToMAC(params["BOOTIF"])
	}

	i.logInterfaces()

	iface, err := i.selectInterface(ctx, bootMAC)
	if err != nil {
		return err
	}

	if ipParam := params["ip"]; ipParam != "" {
		if err := i.configureStaticIP(ctx, iface, ipParam); err != nil {
			return err
		}
	}

	targetDisk, err = i.selectTargetDisk(ctx, targetDisk)
	if err != nil {
		return err
	}

	i.Logger.Printf("target disk: %s", targetDisk)
	i.Logger.Printf("downloading disk image from %s", imageURL)

	if err := retry(ctx, 120, 5*time.Second, "download and write disk image", i.Sleep, i.Logger, func() error {
		return i.downloadAndWriteImage(ctx, imageURL, targetDisk)
	}); err != nil {
		return fmt.Errorf("failed to download and write disk image: %w", err)
	}

	unix.Sync()

	if err := retry(ctx, 5, 2*time.Second, "re-read partition table", i.Sleep, i.Logger, func() error {
		return i.Runner.Run(ctx, "blockdev", "--rereadpt", targetDisk)
	}); err != nil {
		i.Logger.Printf("WARNING: could not re-read partition table")
	}

	i.Sleep(2 * time.Second)

	if dsURL := params["unbounded.ds_url"]; dsURL != "" {
		if err := i.injectCloudInit(ctx, targetDisk, cloudInitConfig{
			DSURL:         dsURL,
			ServeURL:      serveURL,
			NodeName:      params["unbounded.node_name"],
			NodeNamespace: params["unbounded.node_namespace"],
			APIServerURL:  params["unbounded.apiserver_url"],
		}); err != nil {
			return err
		}
	}

	if err := i.createUEFIBootEntry(ctx, targetDisk); err != nil {
		i.Logger.Printf("WARNING: %v", err)
	}

	if serveURL != "" {
		i.Logger.Printf("disabling PXE boot")

		if err := retry(ctx, 5, 2*time.Second, "disable PXE", i.Sleep, i.Logger, func() error {
			return i.disablePXE(ctx, serveURL)
		}); err != nil {
			i.Logger.Printf("WARNING: failed to disable PXE boot")
		}
	}

	i.Logger.Printf("installation complete, rebooting")
	i.Sleep(2 * time.Second)

	if err := retry(ctx, 3, 2*time.Second, "reboot", i.Sleep, i.Logger, func() error {
		return i.Runner.Run(ctx, "reboot", "-f")
	}); err != nil {
		return fmt.Errorf("failed to reboot: %w", err)
	}

	return nil
}

// Fatal logs a fatal error and drops to a shell when one is available, matching
// the old init script's debugging behavior.
func (i *Installer) Fatal(err error) {
	i.normalize()

	if i.Logger == nil {
		i.Logger = NewKernelLogger()
	}

	i.Logger.Printf("FATAL: %v", err)

	shell, lookErr := i.Runner.LookPath("sh")
	if lookErr != nil {
		shell = "/bin/sh"
	}

	if _, statErr := os.Stat(shell); statErr == nil {
		if execErr := syscall.Exec(shell, []string{"sh"}, os.Environ()); execErr != nil {
			i.Logger.Printf("WARNING: failed to exec shell: %v", execErr)
		}
	}

	os.Exit(1)
}

func (i *Installer) setupMounts() error {
	for _, dir := range []string{"/proc", "/sys", "/dev", "/tmp", "/run", i.MountRoot, i.ESPMountPoint} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("creating %s: %w", dir, err)
		}
	}

	mounts := []struct {
		source string
		target string
		fstype string
	}{
		{source: "proc", target: "/proc", fstype: "proc"},
		{source: "sysfs", target: "/sys", fstype: "sysfs"},
		{source: "devtmpfs", target: "/dev", fstype: "devtmpfs"},
	}

	for _, m := range mounts {
		if err := unix.Mount(m.source, m.target, m.fstype, 0, ""); err != nil && !errors.Is(err, unix.EBUSY) {
			// The Ubuntu netboot initrd may already have some pseudo filesystems
			// mounted before this overlay init runs. Keep startup tolerant.
			continue
		}
	}

	return nil
}

func (i *Installer) loadKernelModules(ctx context.Context) error {
	storageModules := []string{"virtio_pci", "virtio_blk", "ahci", "sd_mod", "nvme", "xfs", "ext4"}
	networkModules := []string{"virtio_net", "e1000", "e1000e", "igb", "ixgbe", "i40e", "ice", "mlx5_core", "mlx4_core", "bnxt_en", "tg3", "be2net", "ena"}
	bootModules := []string{"nls_cp437", "nls_ascii", "nls_utf8", "fat", "vfat", "efivarfs"}

	for _, mod := range append(append(storageModules, networkModules...), bootModules...) {
		runBestEffort(ctx, i.Runner, "modprobe", mod)
	}

	kver, err := i.Runner.Output(ctx, "uname", "-r")
	if err != nil {
		return nil
	}

	patterns := []string{
		filepath.Join("/lib/modules", kver, "kernel/drivers/net/ethernet/*/*.ko*"),
		filepath.Join("/lib/modules", kver, "kernel/drivers/net/ethernet/*/*/*.ko*"),
	}

	for _, pattern := range patterns {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			continue
		}

		for _, match := range matches {
			mod := moduleNameFromPath(match)
			if mod != "" {
				runBestEffort(ctx, i.Runner, "modprobe", mod)
			}
		}
	}

	return nil
}

func moduleNameFromPath(path string) string {
	base := filepath.Base(path)

	idx := strings.Index(base, ".ko")
	if idx < 0 {
		return ""
	}

	return base[:idx]
}

func parseCmdline(cmdline string) map[string]string {
	params := make(map[string]string)

	for _, tok := range strings.Fields(cmdline) {
		key, value, ok := strings.Cut(tok, "=")
		if !ok {
			continue
		}

		params[key] = value
	}

	return params
}

func normalizeMAC(mac string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(mac)), "-", ":")
}

func bootifToMAC(bootif string) string {
	value := strings.ToLower(strings.TrimSpace(bootif))
	value = strings.TrimPrefix(value, "01-")

	return strings.ReplaceAll(value, "-", ":")
}

func (i *Installer) logInterfaces() {
	i.Logger.Printf("network interfaces:")

	ifaces, err := i.listInterfaces()
	if err != nil {
		return
	}

	for _, iface := range ifaces {
		i.Logger.Printf("  %s mac=%s state=%s", iface.Name, defaultString(iface.MAC, "unknown"), defaultString(iface.State, "unknown"))
	}
}

type netInterface struct {
	Name  string
	MAC   string
	State string
}

func (i *Installer) listInterfaces() ([]netInterface, error) {
	entries, err := os.ReadDir(filepath.Join(i.SysfsRoot, "class/net"))
	if err != nil {
		return nil, err
	}

	ifaces := make([]netInterface, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		base := filepath.Join(i.SysfsRoot, "class/net", name)
		mac := strings.TrimSpace(readFileString(filepath.Join(base, "address")))
		state := strings.TrimSpace(readFileString(filepath.Join(base, "operstate")))
		ifaces = append(ifaces, netInterface{Name: name, MAC: mac, State: state})
	}

	return ifaces, nil
}

func (i *Installer) selectInterface(ctx context.Context, bootMAC string) (string, error) {
	i.Logger.Printf("waiting for network interface")

	var iface string

	if bootMAC != "" {
		err := retry(ctx, 30, time.Second, "find network interface with MAC "+bootMAC, i.Sleep, i.Logger, func() error {
			name, ok := i.findInterfaceByMAC(bootMAC)
			if !ok {
				return fmt.Errorf("interface with MAC %s not found", bootMAC)
			}

			iface = name

			return nil
		})
		if err != nil {
			i.Logger.Printf("WARNING: no network interface found with MAC %s", bootMAC)
		}
	}

	if iface == "" {
		if err := retry(ctx, 30, time.Second, "find network interface", i.Sleep, i.Logger, func() error {
			name, ok := i.findFirstInterface()
			if !ok {
				return errors.New("no network interface found")
			}

			iface = name

			return nil
		}); err != nil {
			return "", errors.New("no network interface found")
		}

		i.Logger.Printf("WARNING: using first non-loopback network interface %s", iface)
	} else {
		i.Logger.Printf("selected network interface %s for MAC %s", iface, bootMAC)
	}

	return iface, nil
}

func (i *Installer) findInterfaceByMAC(mac string) (string, bool) {
	ifaces, err := i.listInterfaces()
	if err != nil {
		return "", false
	}

	for _, iface := range ifaces {
		if iface.Name == "lo" {
			continue
		}

		if normalizeMAC(iface.MAC) == mac {
			return iface.Name, true
		}
	}

	return "", false
}

func (i *Installer) findFirstInterface() (string, bool) {
	ifaces, err := i.listInterfaces()
	if err != nil {
		return "", false
	}

	for _, iface := range ifaces {
		if iface.Name != "lo" {
			return iface.Name, true
		}
	}

	return "", false
}

type ipConfig struct {
	ClientIP string
	Gateway  string
	Prefix   string
	Iface    string
}

func parseIPParam(value string) ipConfig {
	fields := strings.Split(value, ":")
	field := func(idx int) string {
		if idx >= len(fields) {
			return ""
		}

		return fields[idx]
	}

	mask := field(3)

	prefix := mask
	if strings.Contains(mask, ".") {
		prefix = maskToCIDR(mask)
	}

	return ipConfig{
		ClientIP: field(0),
		Gateway:  field(2),
		Prefix:   prefix,
		Iface:    field(5),
	}
}

func maskToCIDR(mask string) string {
	cidr := 0

	for _, octet := range strings.Split(mask, ".") {
		switch octet {
		case "255":
			cidr += 8
		case "254":
			cidr += 7
		case "252":
			cidr += 6
		case "248":
			cidr += 5
		case "240":
			cidr += 4
		case "224":
			cidr += 3
		case "192":
			cidr += 2
		case "128":
			cidr++
		}
	}

	return strconv.Itoa(cidr)
}

func (i *Installer) configureStaticIP(ctx context.Context, selectedIface, value string) error {
	cfg := parseIPParam(value)

	iface := selectedIface
	if cfg.Iface != "" && pathExists(filepath.Join(i.SysfsRoot, "class/net", cfg.Iface)) {
		iface = cfg.Iface
	}

	i.Logger.Printf("configuring %s with %s/%s gw %s", iface, cfg.ClientIP, cfg.Prefix, cfg.Gateway)

	if err := retry(ctx, 5, 2*time.Second, "link up "+iface, i.Sleep, i.Logger, func() error {
		return i.Runner.Run(ctx, "ip", "link", "set", iface, "up")
	}); err != nil {
		return fmt.Errorf("failed to bring up %s: %w", iface, err)
	}

	if err := retry(ctx, 3, time.Second, "add address", i.Sleep, i.Logger, func() error {
		return i.Runner.Run(ctx, "ip", "addr", "add", cfg.ClientIP+"/"+cfg.Prefix, "dev", iface)
	}); err != nil {
		i.Logger.Printf("WARNING: failed to add address")
	}

	if cfg.Gateway != "" {
		if err := retry(ctx, 3, time.Second, "add default route", i.Sleep, i.Logger, func() error {
			return i.Runner.Run(ctx, "ip", "route", "add", "default", "via", cfg.Gateway, "dev", iface)
		}); err != nil {
			i.Logger.Printf("WARNING: failed to add default route")
		}
	}

	return nil
}

func (i *Installer) selectTargetDisk(ctx context.Context, configured string) (string, error) {
	if configured == "" {
		i.Logger.Printf("waiting for block devices")
		i.logDisks()

		var disk string

		if err := retry(ctx, 30, time.Second, "find block device", i.Sleep, i.Logger, func() error {
			selected, ok := i.findLargestDisk()
			if !ok {
				return errors.New("no target disk found")
			}

			disk = selected

			return nil
		}); err != nil {
			return "", errors.New("no target disk found")
		}

		i.Logger.Printf("WARNING: target disk was not specified, selected largest disk")

		configured = disk
	} else if !pathExists(configured) {
		return "", fmt.Errorf("target disk %s does not exist", configured)
	}

	resolved, err := filepath.EvalSymlinks(configured)
	if err == nil {
		configured = resolved
	}

	if _, err := i.targetDiskSysfs(configured); err != nil {
		return "", err
	}

	return configured, nil
}

func (i *Installer) logDisks() {
	i.Logger.Printf("candidate disks:")

	for _, disk := range i.candidateDisks() {
		size := strings.TrimSpace(readFileString(filepath.Join(disk.SysfsPath, "size")))
		model := strings.TrimSpace(readFileString(filepath.Join(disk.SysfsPath, "device/model")))
		serial := strings.TrimSpace(readFileString(filepath.Join(disk.SysfsPath, "device/serial")))
		removable := strings.TrimSpace(readFileString(filepath.Join(disk.SysfsPath, "removable")))
		i.Logger.Printf("  /dev/%s sectors=%s model=%s serial=%s removable=%s", disk.Name, defaultString(size, "0"), defaultString(model, "unknown"), defaultString(serial, "unknown"), defaultString(removable, "unknown"))
	}
}

type diskCandidate struct {
	Name      string
	SysfsPath string
	Sectors   uint64
}

func (i *Installer) candidateDisks() []diskCandidate {
	patterns := []string{
		filepath.Join(i.SysfsRoot, "block/sd*"),
		filepath.Join(i.SysfsRoot, "block/nvme*n*"),
		filepath.Join(i.SysfsRoot, "block/vd*"),
	}

	var disks []diskCandidate

	for _, pattern := range patterns {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			continue
		}

		for _, match := range matches {
			if !isWholeDiskSysfs(match) {
				continue
			}

			sectors, err := strconv.ParseUint(strings.TrimSpace(readFileString(filepath.Join(match, "size"))), 10, 64)
			if err != nil {
				sectors = 0
			}

			disks = append(disks, diskCandidate{Name: filepath.Base(match), SysfsPath: match, Sectors: sectors})
		}
	}

	return disks
}

func isWholeDiskSysfs(path string) bool {
	return !pathExists(filepath.Join(path, "partition"))
}

func (i *Installer) findLargestDisk() (string, bool) {
	var selected diskCandidate
	for _, disk := range i.candidateDisks() {
		if disk.Sectors > selected.Sectors {
			selected = disk
		}
	}

	if selected.Name == "" {
		return "", false
	}

	return "/dev/" + selected.Name, true
}

func (i *Installer) targetDiskSysfs(targetDisk string) (string, error) {
	base := filepath.Base(targetDisk)

	sysdisk := filepath.Join(i.SysfsRoot, "class/block", base)
	if resolved, err := filepath.EvalSymlinks(sysdisk); err == nil {
		sysdisk = resolved
	} else {
		sysdisk = filepath.Join(i.SysfsRoot, "block", base)
	}

	if !pathExists(sysdisk) {
		return "", fmt.Errorf("target disk %s has no sysfs device", targetDisk)
	}

	if pathExists(filepath.Join(sysdisk, "partition")) {
		return "", fmt.Errorf("target disk %s is a partition, expected whole disk", targetDisk)
	}

	return sysdisk, nil
}

func (i *Installer) partsForDisk(targetDisk string) []string {
	sysdisk, err := i.targetDiskSysfs(targetDisk)
	if err != nil {
		return nil
	}

	entries, err := os.ReadDir(sysdisk)
	if err != nil {
		return nil
	}

	parts := make([]string, 0, len(entries))
	for _, entry := range entries {
		if pathExists(filepath.Join(sysdisk, entry.Name(), "partition")) {
			parts = append(parts, "/dev/"+entry.Name())
		}
	}

	return parts
}

func (i *Installer) partitionNumber(part string) string {
	return strings.TrimSpace(readFileString(filepath.Join(i.SysfsRoot, "class/block", filepath.Base(part), "partition")))
}

func (i *Installer) downloadAndWriteImage(ctx context.Context, imageURL, targetDisk string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, imageURL, nil)
	if err != nil {
		return err
	}

	resp, err := i.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer closeBestEffort(resp.Body)

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("GET %s returned %s", imageURL, resp.Status)
	}

	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		return fmt.Errorf("opening gzip stream: %w", err)
	}
	defer closeBestEffort(gz)

	out, err := os.OpenFile(targetDisk, os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("opening target disk: %w", err)
	}
	defer closeBestEffort(out)

	buf := make([]byte, 4*1024*1024)
	if _, err := io.CopyBuffer(out, gz, buf); err != nil {
		return fmt.Errorf("writing target disk: %w", err)
	}

	if err := out.Sync(); err != nil {
		return fmt.Errorf("syncing target disk: %w", err)
	}

	return nil
}

type cloudInitConfig struct {
	DSURL         string
	ServeURL      string
	NodeName      string
	NodeNamespace string
	APIServerURL  string
}

func (i *Installer) injectCloudInit(ctx context.Context, targetDisk string, cfg cloudInitConfig) error {
	i.Logger.Printf("injecting cloud-init datasource: %s", cfg.DSURL)

	var rootPart string

	if err := retry(ctx, 20, 2*time.Second, "find root partition", i.Sleep, i.Logger, func() error {
		part, ok := i.findRootPartition(ctx, targetDisk)
		if !ok {
			return errors.New("no root partition found")
		}

		rootPart = part

		return nil
	}); err != nil {
		return fmt.Errorf("no root partition found on %s: %w", targetDisk, err)
	}

	if err := retry(ctx, 5, 2*time.Second, "mount "+rootPart, i.Sleep, i.Logger, func() error {
		return i.Runner.Run(ctx, "mount", rootPart, i.MountRoot)
	}); err != nil {
		return fmt.Errorf("failed to mount %s: %w", rootPart, err)
	}

	mounted := true
	defer func() {
		if mounted {
			runBestEffort(context.Background(), i.Runner, "umount", i.MountRoot)
		}
	}()

	if err := os.MkdirAll(filepath.Join(i.MountRoot, "etc/cloud/cloud.cfg.d"), 0o755); err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Join(i.MountRoot, "etc/metalman"), 0o755); err != nil {
		return err
	}

	if err := os.WriteFile(filepath.Join(i.MountRoot, "etc/cloud/cloud.cfg.d/99-metalman.cfg"), []byte(renderNoCloudConfig(cfg.DSURL)), 0o644); err != nil {
		return err
	}

	if err := os.Remove(filepath.Join(i.MountRoot, "etc/cloud/cloud-init.disabled")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	if err := os.WriteFile(filepath.Join(i.MountRoot, "etc/metalman/config"), []byte(renderMetalmanConfig(cfg)), 0o644); err != nil {
		return err
	}

	unix.Sync()

	if err := i.Runner.Run(ctx, "umount", i.MountRoot); err != nil {
		return err
	}

	mounted = false

	i.Logger.Printf("cloud-init configured on %s", rootPart)

	return nil
}

func renderNoCloudConfig(dsURL string) string {
	return fmt.Sprintf("datasource_list: [NoCloud]\ndatasource:\n  NoCloud:\n    seedfrom: %s\n", dsURL)
}

func renderMetalmanConfig(cfg cloudInitConfig) string {
	return fmt.Sprintf("SERVE_URL=%s\nNODE_NAME=%s\nNODE_NAMESPACE=%s\nAPISERVER_URL=%s\n", cfg.ServeURL, cfg.NodeName, cfg.NodeNamespace, cfg.APIServerURL)
}

func (i *Installer) findRootPartition(ctx context.Context, targetDisk string) (string, bool) {
	for _, part := range i.partsForDisk(targetDisk) {
		if err := i.Runner.Run(ctx, "mount", part, i.MountRoot); err != nil {
			continue
		}

		isRoot := pathExists(filepath.Join(i.MountRoot, "etc")) && pathExists(filepath.Join(i.MountRoot, "var"))
		runBestEffort(ctx, i.Runner, "umount", i.MountRoot)

		if isRoot {
			return part, true
		}
	}

	return "", false
}

func (i *Installer) createUEFIBootEntry(ctx context.Context, targetDisk string) error {
	if !pathExists(filepath.Join(i.SysfsRoot, "firmware/efi")) {
		return nil
	}

	i.Logger.Printf("creating UEFI boot entry for local disk")

	efivars := filepath.Join(i.SysfsRoot, "firmware/efi/efivars")
	if err := os.MkdirAll(efivars, 0o755); err != nil {
		i.Logger.Printf("WARNING: creating efivars mount point failed: %v", err)
	}

	if err := unix.Mount("efivarfs", efivars, "efivarfs", 0, ""); err != nil && !errors.Is(err, unix.EBUSY) {
		i.Logger.Printf("WARNING: efivarfs mount failed: %v", err)
	}

	for _, part := range i.partsForDisk(targetDisk) {
		if err := i.Runner.Run(ctx, "mount", "-t", "vfat", part, i.ESPMountPoint); err != nil {
			continue
		}

		loader := findEFILoader(i.ESPMountPoint)
		runBestEffort(ctx, i.Runner, "umount", i.ESPMountPoint)

		if loader == "" {
			continue
		}

		if _, err := i.Runner.LookPath("efibootmgr"); err != nil {
			continue
		}

		espNum := i.partitionNumber(part)
		if espNum == "" {
			continue
		}

		if err := i.Runner.Run(ctx, "efibootmgr", "--create", "--disk", targetDisk, "--part", espNum, "--loader", loader, "--label", "metalman"); err != nil {
			i.Logger.Printf("WARNING: efibootmgr failed, PXE chainloader will be used as fallback")
		} else {
			i.Logger.Printf("UEFI boot entry created (%s on part %s)", loader, espNum)
		}

		break
	}

	return nil
}

func findEFILoader(mountPoint string) string {
	candidates := []string{
		"/EFI/BOOT/BOOTX64.EFI",
		"/EFI/BOOT/BOOTAA64.EFI",
		"/EFI/ubuntu/shimx64.efi",
		"/EFI/ubuntu/shimaa64.efi",
	}

	for _, candidate := range candidates {
		if pathExists(filepath.Join(mountPoint, candidate)) {
			return strings.ReplaceAll(candidate, "/", "\\")
		}
	}

	return ""
}

func (i *Installer) disablePXE(ctx context.Context, serveURL string) error {
	url := strings.TrimRight(serveURL, "/") + "/pxe/disable"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}

	resp, err := i.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer closeBestEffort(resp.Body)

	io.Copy(io.Discard, resp.Body) //nolint:errcheck // Drain best-effort before closing.

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("GET %s returned %s", url, resp.Status)
	}

	return nil
}

func retry(ctx context.Context, attempts int, delay time.Duration, desc string, sleep func(time.Duration), log *Logger, fn func() error) error {
	var lastErr error

	for attempt := 1; attempt <= attempts; attempt++ {
		if err := fn(); err == nil {
			if attempt > 1 {
				log.Printf("%s succeeded (attempt %d/%d)", desc, attempt, attempts)
			}

			return nil
		} else {
			lastErr = err
		}

		if attempt == attempts {
			return lastErr
		}

		log.Printf("%s failed (attempt %d/%d), retrying in %ds", desc, attempt, attempts, int(delay.Seconds()))

		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			sleep(delay)
		}
	}

	return lastErr
}

func readFileString(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}

	return string(data)
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func defaultString(value, fallback string) string {
	if value == "" {
		return fallback
	}

	return value
}
