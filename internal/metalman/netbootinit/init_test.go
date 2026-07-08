// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package netbootinit

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseCmdline(t *testing.T) {
	params := parseCmdline("BOOTIF=01-AA-BB-CC-DD-EE-FF quiet console=ttyS0 unbounded.image_url=http://10.0.0.1/disk.img.gz ip=10.0.0.5::10.0.0.1:255.255.255.0:::none")

	if params["BOOTIF"] != "01-AA-BB-CC-DD-EE-FF" {
		t.Fatalf("BOOTIF = %q", params["BOOTIF"])
	}

	if params["unbounded.image_url"] != "http://10.0.0.1/disk.img.gz" {
		t.Fatalf("image url = %q", params["unbounded.image_url"])
	}

	if params["console"] != "ttyS0" {
		t.Fatalf("console = %q", params["console"])
	}

	if _, ok := params["quiet"]; ok {
		t.Fatal("quiet token without value should not be included")
	}
}

func TestMACNormalization(t *testing.T) {
	if got := normalizeMAC("AA-BB-CC-DD-EE-FF"); got != "aa:bb:cc:dd:ee:ff" {
		t.Fatalf("normalizeMAC = %q", got)
	}

	if got := bootifToMAC("01-AA-BB-CC-DD-EE-FF"); got != "aa:bb:cc:dd:ee:ff" {
		t.Fatalf("bootifToMAC = %q", got)
	}
}

func TestInstallConfigFromCmdline(t *testing.T) {
	cfg, err := installConfigFromCmdline(strings.Join([]string{
		"BOOTIF=01-AA-BB-CC-DD-EE-FF",
		"unbounded.image_url=http://10.0.0.1/disk.img.gz",
		"unbounded.serve_url=http://10.0.0.1",
		"unbounded.disk=/dev/nvme0n1",
		"unbounded.ds_url=http://10.0.0.1/cloud-init/",
		"unbounded.node_name=node-1",
		"unbounded.node_namespace=default",
		"unbounded.apiserver_url=https://api.example.com",
		"ip=10.0.0.10::10.0.0.1:255.255.255.0:::none",
	}, " "))
	if err != nil {
		t.Fatal(err)
	}

	if cfg.ImageURL != "http://10.0.0.1/disk.img.gz" {
		t.Fatalf("image url = %q", cfg.ImageURL)
	}

	if cfg.ServeURL != "http://10.0.0.1" {
		t.Fatalf("serve url = %q", cfg.ServeURL)
	}

	if cfg.TargetDisk != "/dev/nvme0n1" {
		t.Fatalf("target disk = %q", cfg.TargetDisk)
	}

	if cfg.BootMAC != "aa:bb:cc:dd:ee:ff" {
		t.Fatalf("boot MAC = %q", cfg.BootMAC)
	}

	if cfg.IPParam != "10.0.0.10::10.0.0.1:255.255.255.0:::none" {
		t.Fatalf("ip param = %q", cfg.IPParam)
	}

	wantCloudInit := cloudInitConfig{
		DSURL:         "http://10.0.0.1/cloud-init/",
		ServeURL:      "http://10.0.0.1",
		NodeName:      "node-1",
		NodeNamespace: "default",
		APIServerURL:  "https://api.example.com",
	}
	if cfg.CloudInit != wantCloudInit {
		t.Fatalf("cloud-init config = %#v, want %#v", cfg.CloudInit, wantCloudInit)
	}
}

func TestInstallConfigFromCmdlinePrefersExplicitBootMAC(t *testing.T) {
	cfg, err := installConfigFromCmdline("BOOTIF=01-AA-BB-CC-DD-EE-FF unbounded.boot_mac=11-22-33-44-55-66 unbounded.image_url=http://10.0.0.1/disk.img.gz")
	if err != nil {
		t.Fatal(err)
	}

	if cfg.BootMAC != "11:22:33:44:55:66" {
		t.Fatalf("boot MAC = %q", cfg.BootMAC)
	}
}

func TestInstallConfigFromCmdlineRequiresImageURL(t *testing.T) {
	if _, err := installConfigFromCmdline("BOOTIF=01-AA-BB-CC-DD-EE-FF"); err == nil {
		t.Fatal("expected missing image URL error")
	}
}

func TestParseIPParam(t *testing.T) {
	cfg, err := parseIPParam("10.0.0.10::10.0.0.1:255.255.254.0:node-1:eth1:none")
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Address.String() != "10.0.0.10/23" {
		t.Fatalf("address = %s", cfg.Address.String())
	}

	if !cfg.Gateway.Equal(net.ParseIP("10.0.0.1")) {
		t.Fatalf("gateway = %s", cfg.Gateway.String())
	}

	if cfg.Iface != "eth1" {
		t.Fatalf("iface = %q", cfg.Iface)
	}

	if got, err := maskToCIDR("255.255.255.128"); err != nil || got != 25 {
		t.Fatalf("maskToCIDR = %d", got)
	}
}

func TestParseIPParamRejectsInvalidValues(t *testing.T) {
	for _, value := range []string{
		"dhcp",
		"not-ip::10.0.0.1:255.255.255.0:::none",
		"10.0.0.10::bad-gw:255.255.255.0:::none",
		"10.0.0.10::10.0.0.1:255.0.255.0:::none",
	} {
		if _, err := parseIPParam(value); err == nil {
			t.Fatalf("parseIPParam(%q) succeeded, want error", value)
		}
	}
}

func TestSelectInterface(t *testing.T) {
	sysfs := t.TempDir()
	writeSysfsFile(t, sysfs, "class/net/lo/address", "00:00:00:00:00:00\n")
	writeSysfsFile(t, sysfs, "class/net/lo/operstate", "unknown\n")
	writeSysfsFile(t, sysfs, "class/net/eno1/address", "AA:BB:CC:DD:EE:01\n")
	writeSysfsFile(t, sysfs, "class/net/eno1/operstate", "up\n")
	writeSysfsFile(t, sysfs, "class/net/eno2/address", "aa:bb:cc:dd:ee:02\n")
	writeSysfsFile(t, sysfs, "class/net/eno2/operstate", "down\n")

	installer := testInstaller(sysfs)

	iface, err := installer.selectInterface(context.Background(), "aa:bb:cc:dd:ee:02")
	if err != nil {
		t.Fatal(err)
	}

	if iface != "eno2" {
		t.Fatalf("iface = %q", iface)
	}

	iface, err = installer.selectInterface(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}

	if iface != "eno1" {
		t.Fatalf("fallback iface = %q", iface)
	}
}

func TestFindLargestDisk(t *testing.T) {
	sysfs := t.TempDir()
	writeSysfsFile(t, sysfs, "block/sda/size", "100\n")
	writeSysfsFile(t, sysfs, "block/sda/removable", "0\n")
	writeSysfsFile(t, sysfs, "block/nvme0n1/size", "200\n")
	writeSysfsFile(t, sysfs, "block/vda/partition", "1\n")
	writeSysfsFile(t, sysfs, "block/vda/size", "300\n")

	installer := testInstaller(sysfs)

	disk, ok := installer.findLargestDisk()
	if !ok {
		t.Fatal("expected disk")
	}

	if disk != "/dev/nvme0n1" {
		t.Fatalf("disk = %q", disk)
	}
}

func TestPartsForDisk(t *testing.T) {
	sysfs := t.TempDir()
	writeSysfsFile(t, sysfs, "block/nvme0n1/size", "200\n")
	writeSysfsFile(t, sysfs, "block/nvme0n1/nvme0n1p1/partition", "1\n")
	writeSysfsFile(t, sysfs, "block/nvme0n1/nvme0n1p2/partition", "2\n")
	writeSysfsFile(t, sysfs, "block/nvme0n1/queue/scheduler", "none\n")

	installer := testInstaller(sysfs)
	parts := installer.partitionsForDisk("/dev/nvme0n1")

	want := []partition{{Device: "/dev/nvme0n1p1", Number: "1"}, {Device: "/dev/nvme0n1p2", Number: "2"}}
	if !reflect.DeepEqual(parts, want) {
		t.Fatalf("parts = %#v, want %#v", parts, want)
	}
}

func TestRenderCloudInitConfig(t *testing.T) {
	if got := renderNoCloudConfig("http://10.0.0.1/cloud-init/"); got != "datasource_list: [NoCloud]\ndatasource:\n  NoCloud:\n    seedfrom: http://10.0.0.1/cloud-init/\n" {
		t.Fatalf("NoCloud config = %q", got)
	}

	got := renderMetalmanConfig(cloudInitConfig{
		ServeURL:      "http://10.0.0.1",
		NodeName:      "node-1",
		NodeNamespace: "default",
		APIServerURL:  "https://api.example.com",
	})

	want := "SERVE_URL=http://10.0.0.1\nNODE_NAME=node-1\nNODE_NAMESPACE=default\nAPISERVER_URL=https://api.example.com\n"
	if got != want {
		t.Fatalf("metalman config = %q, want %q", got, want)
	}
}

func TestWriteCloudInitFilesUsesOriginalContractPaths(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "etc/cloud/cloud-init.disabled"), "")

	err := writeCloudInitFiles(root, cloudInitConfig{
		DSURL:         "http://10.0.0.1/cloud-init/",
		ServeURL:      "http://10.0.0.1",
		NodeName:      "node-1",
		NodeNamespace: "default",
		APIServerURL:  "https://api.example.com",
	})
	if err != nil {
		t.Fatal(err)
	}

	assertFile(t, filepath.Join(root, "etc/cloud/cloud.cfg.d/99-unbounded-metal.cfg"), "datasource_list: [NoCloud]\ndatasource:\n  NoCloud:\n    seedfrom: http://10.0.0.1/cloud-init/\n")
	assertFile(t, filepath.Join(root, "etc/unbounded-metal/config"), "SERVE_URL=http://10.0.0.1\nNODE_NAME=node-1\nNODE_NAMESPACE=default\nAPISERVER_URL=https://api.example.com\n")

	if pathExists(filepath.Join(root, "etc/cloud/cloud-init.disabled")) {
		t.Fatal("cloud-init.disabled still exists")
	}
}

func TestDownloadAndWriteImage(t *testing.T) {
	content := []byte("raw disk image content")

	var gzipped bytes.Buffer

	gz := gzip.NewWriter(&gzipped)
	if _, err := gz.Write(content); err != nil {
		t.Fatal(err)
	}

	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write(gzipped.Bytes()) //nolint:errcheck // Test response write.
	}))
	defer server.Close()

	target := filepath.Join(t.TempDir(), "disk.img")
	if err := os.WriteFile(target, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	installer := testInstaller(t.TempDir())
	installer.HTTPClient = server.Client()

	if err := installer.downloadAndWriteImage(context.Background(), server.URL, target); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(got, content) {
		t.Fatalf("target content = %q, want %q", got, content)
	}
}

func TestFindEFILoader(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "EFI/ubuntu/shimx64.efi"), "shim")

	if got := findEFILoader(dir); got != `\EFI\ubuntu\shimx64.efi` {
		t.Fatalf("loader = %q", got)
	}
}

func TestConfigureStaticIPUsesNetworkConfigurator(t *testing.T) {
	sysfs := t.TempDir()
	writeSysfsFile(t, sysfs, "class/net/eth1/address", "aa:bb:cc:dd:ee:ff\n")

	network := &recordingNetwork{}
	installer := testInstaller(sysfs)
	installer.Network = network

	if err := installer.configureStaticIP(context.Background(), "eth0", "10.0.0.10::10.0.0.1:255.255.255.0::eth1:none"); err != nil {
		t.Fatal(err)
	}

	want := []string{"up eth1", "addr eth1 10.0.0.10/24", "route eth1 10.0.0.1"}
	if !reflect.DeepEqual(network.calls, want) {
		t.Fatalf("network calls = %#v, want %#v", network.calls, want)
	}
}

func TestRetryStopsOnCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := retry(ctx, 3, time.Second, "test", sleepContext, NewLogger(ioDiscard{}), func() error {
		return errors.New("fail")
	})
	if err == nil || !strings.Contains(err.Error(), context.Canceled.Error()) {
		t.Fatalf("retry err = %v, want canceled", err)
	}
}

func testInstaller(sysfs string) *Installer {
	return &Installer{
		SysfsRoot:     sysfs,
		MountRoot:     filepath.Join(os.TempDir(), "metalman-test-mnt"),
		ESPMountPoint: filepath.Join(os.TempDir(), "metalman-test-esp"),
		Logger:        NewLogger(ioDiscard{}),
		Runner:        fakeRunner{},
		System:        fakeSystem{},
		Network:       fakeNetwork{},
		HTTPClient:    http.DefaultClient,
		Sleep:         func(context.Context, time.Duration) error { return nil },
	}
}

type ioDiscard struct{}

func (ioDiscard) Write(p []byte) (int, error) { return len(p), nil }

type fakeRunner struct{}

func (fakeRunner) Run(context.Context, string, ...string) error { return nil }

func (fakeRunner) Output(context.Context, string, ...string) (string, error) { return "", nil }

func (fakeRunner) LookPath(name string) (string, error) { return name, nil }

type fakeSystem struct{}

func (fakeSystem) KernelRelease() (string, error) { return "test-kernel", nil }

func (fakeSystem) Mount(_, _, _ string) error { return nil }

func (fakeSystem) Unmount(string) error { return nil }

func (fakeSystem) RereadPartitionTable(string) error { return nil }

func (fakeSystem) Reboot() error { return nil }

func (fakeSystem) Sync() {}

type fakeNetwork struct{}

func (fakeNetwork) LinkSetUp(string) error { return nil }

func (fakeNetwork) AddrAdd(string, *net.IPNet) error { return nil }

func (fakeNetwork) RouteAddDefault(string, net.IP) error { return nil }

type recordingNetwork struct {
	calls []string
}

func (r *recordingNetwork) LinkSetUp(name string) error {
	r.calls = append(r.calls, "up "+name)
	return nil
}

func (r *recordingNetwork) AddrAdd(name string, ipNet *net.IPNet) error {
	r.calls = append(r.calls, "addr "+name+" "+ipNet.String())
	return nil
}

func (r *recordingNetwork) RouteAddDefault(name string, gateway net.IP) error {
	r.calls = append(r.calls, "route "+name+" "+gateway.String())
	return nil
}

func writeSysfsFile(t *testing.T, root, rel, content string) {
	t.Helper()
	writeFile(t, filepath.Join(root, rel), content)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func assertFile(t *testing.T, path, want string) {
	t.Helper()

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if string(got) != want {
		t.Fatalf("%s = %q, want %q", path, string(got), want)
	}
}
