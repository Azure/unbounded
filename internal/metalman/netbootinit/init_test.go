// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package netbootinit

import (
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
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

func TestParseIPParam(t *testing.T) {
	cfg := parseIPParam("10.0.0.10::10.0.0.1:255.255.254.0:node-1:eth1:none")

	want := ipConfig{ClientIP: "10.0.0.10", Gateway: "10.0.0.1", Prefix: "23", Iface: "eth1"}
	if cfg != want {
		t.Fatalf("parseIPParam = %#v, want %#v", cfg, want)
	}

	if got := maskToCIDR("255.255.255.128"); got != "25" {
		t.Fatalf("maskToCIDR = %q", got)
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
	parts := installer.partsForDisk("/dev/nvme0n1")

	want := []string{"/dev/nvme0n1p1", "/dev/nvme0n1p2"}
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

func testInstaller(sysfs string) *Installer {
	return &Installer{
		SysfsRoot:     sysfs,
		MountRoot:     filepath.Join(os.TempDir(), "metalman-test-mnt"),
		ESPMountPoint: filepath.Join(os.TempDir(), "metalman-test-esp"),
		Logger:        NewLogger(ioDiscard{}),
		Runner:        fakeRunner{},
		HTTPClient:    http.DefaultClient,
		Sleep:         func(time.Duration) {},
	}
}

type ioDiscard struct{}

func (ioDiscard) Write(p []byte) (int, error) { return len(p), nil }

type fakeRunner struct{}

func (fakeRunner) Run(context.Context, string, ...string) error { return nil }

func (fakeRunner) Output(context.Context, string, ...string) (string, error) { return "", nil }

func (fakeRunner) LookPath(name string) (string, error) { return name, nil }

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
