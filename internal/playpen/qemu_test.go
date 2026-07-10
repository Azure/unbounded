// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package playpen

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildQEMUArgsAMD64PXEBoot(t *testing.T) {
	cfg := normalizedTestConfig(t, ArchAMD64)

	args, err := BuildQEMUArgs(cfg, 3, Firmware{})
	if err != nil {
		t.Fatalf("BuildQEMUArgs() error = %v", err)
	}

	joined := strings.Join(args, " ")
	for _, want := range []string{
		"-enable-kvm",
		"q35,accel=kvm",
		"-cpu host",
		"tpm-tis,tpmdev=tpm0",
		"socket,id=chrtpm,path=/run/playpen/swtpm.sock",
		"emulator,id=tpm0,chardev=chrtpm",
		"-boot menu=on,strict=on",
		"if=none,id=disk0,format=raw,file=/var/lib/playpen/disk.raw",
		"virtio-blk-pci,drive=disk0,bootindex=2",
		"tap,id=net0,fd=3",
		"virtio-net-pci,netdev=net0,mac=02:00:00:00:00:01,bootindex=1",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("QEMU args %q do not contain %q", joined, want)
		}
	}

	for _, forbidden := range []string{"-kernel", "-initrd", "-initramfs", "-append"} {
		if containsArg(args, forbidden) {
			t.Fatalf("QEMU args contain forbidden boot artifact arg %q: %v", forbidden, args)
		}
	}

	if driveCount := countArg(args, "-drive"); driveCount != 1 {
		t.Fatalf("-drive count = %d, want 1 guest disk", driveCount)
	}
}

func TestBuildQEMUArgsARM64FirmwarePXEBoot(t *testing.T) {
	cfg := normalizedTestConfig(t, ArchARM64)
	firmware := Firmware{
		CodePath: "/firmware/AAVMF_CODE.fd",
		VarsPath: "/run/playpen/AAVMF_VARS.fd",
	}

	args, err := BuildQEMUArgs(cfg, 3, firmware)
	if err != nil {
		t.Fatalf("BuildQEMUArgs() error = %v", err)
	}

	joined := strings.Join(args, " ")
	for _, want := range []string{
		"-enable-kvm",
		"virt,accel=kvm,gic-version=host",
		"-cpu host",
		"tpm-tis-device,tpmdev=tpm0",
		"socket,id=chrtpm,path=/run/playpen/swtpm.sock",
		"emulator,id=tpm0,chardev=chrtpm",
		"if=pflash,format=raw,readonly=on,file=/firmware/AAVMF_CODE.fd",
		"if=pflash,format=raw,file=/run/playpen/AAVMF_VARS.fd",
		"-boot menu=on,strict=on",
		"if=none,id=disk0,format=raw,file=/var/lib/playpen/disk.raw",
		"virtio-blk-pci,drive=disk0,bootindex=2",
		"virtio-net-pci,netdev=net0,mac=02:00:00:00:00:01,bootindex=1",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("QEMU args %q do not contain %q", joined, want)
		}
	}

	for _, forbidden := range []string{"-kernel", "-initrd", "-initramfs", "-append"} {
		if containsArg(args, forbidden) {
			t.Fatalf("QEMU args contain forbidden boot artifact arg %q: %v", forbidden, args)
		}
	}

	if driveCount := countArg(args, "-drive"); driveCount != 3 {
		t.Fatalf("-drive count = %d, want 2 pflash drives and 1 guest disk", driveCount)
	}
}

func TestBuildQEMUArgsEscapesDiskPathComma(t *testing.T) {
	cfg := normalizedTestConfig(t, ArchAMD64)
	cfg.DiskPath = "/var/lib/playpen/disk,one.raw"

	args, err := BuildQEMUArgs(cfg, 3, Firmware{})
	if err != nil {
		t.Fatalf("BuildQEMUArgs() error = %v", err)
	}

	if joined := strings.Join(args, " "); !strings.Contains(joined, "file=/var/lib/playpen/disk,,one.raw") {
		t.Fatalf("QEMU args did not escape disk path comma: %v", args)
	}
}

func TestBuildQEMUArgsEscapesTPMSocketComma(t *testing.T) {
	cfg := normalizedTestConfig(t, ArchAMD64)
	cfg.TPMSocket = "/run/playpen/swtpm,one.sock"

	args, err := BuildQEMUArgs(cfg, 3, Firmware{})
	if err != nil {
		t.Fatalf("BuildQEMUArgs() error = %v", err)
	}

	if joined := strings.Join(args, " "); !strings.Contains(joined, "path=/run/playpen/swtpm,,one.sock") {
		t.Fatalf("QEMU args did not escape TPM socket comma: %v", args)
	}
}

func TestPrepareDiskCreatesSparseDisk(t *testing.T) {
	diskPath := filepath.Join(t.TempDir(), "nested", "disk.raw")
	cfg := Config{DiskPath: diskPath, DiskSize: "2MB"}

	if err := PrepareDisk(cfg); err != nil {
		t.Fatalf("PrepareDisk() error = %v", err)
	}

	info, err := os.Stat(diskPath)
	if err != nil {
		t.Fatalf("stat disk: %v", err)
	}

	if info.Size() != 2<<20 {
		t.Fatalf("disk size = %d, want %d", info.Size(), 2<<20)
	}

	if info.Mode().Perm() != 0o600 {
		t.Fatalf("disk mode = %o, want 600", info.Mode().Perm())
	}
}

func TestPrepareDiskPreservesExistingDisk(t *testing.T) {
	diskPath := filepath.Join(t.TempDir(), "disk.raw")

	want := []byte("installed guest data")
	if err := os.WriteFile(diskPath, want, 0o600); err != nil {
		t.Fatalf("write existing disk: %v", err)
	}

	if err := PrepareDisk(Config{DiskPath: diskPath, DiskSize: "4M"}); err != nil {
		t.Fatalf("PrepareDisk() error = %v", err)
	}

	got, err := os.ReadFile(diskPath)
	if err != nil {
		t.Fatalf("read existing disk: %v", err)
	}

	if string(got) != string(want) {
		t.Fatalf("disk contents = %q, want %q", got, want)
	}
}

func TestPrepareDiskInitializesEmptyExistingDisk(t *testing.T) {
	diskPath := filepath.Join(t.TempDir(), "disk.raw")
	if err := os.WriteFile(diskPath, nil, 0o600); err != nil {
		t.Fatalf("create empty disk: %v", err)
	}

	if err := PrepareDisk(Config{DiskPath: diskPath, DiskSize: "1M"}); err != nil {
		t.Fatalf("PrepareDisk() error = %v", err)
	}

	info, err := os.Stat(diskPath)
	if err != nil {
		t.Fatalf("stat disk: %v", err)
	}

	if info.Size() != 1<<20 {
		t.Fatalf("disk size = %d, want %d", info.Size(), 1<<20)
	}
}

func TestPrepareDiskRejectsInvalidTarget(t *testing.T) {
	parentFile := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(parentFile, []byte("data"), 0o600); err != nil {
		t.Fatalf("write parent file: %v", err)
	}

	err := PrepareDisk(Config{DiskPath: filepath.Join(parentFile, "disk.raw"), DiskSize: "1M"})
	if err == nil || !strings.Contains(err.Error(), "create disk directory") {
		t.Fatalf("PrepareDisk() error = %v, want disk directory error", err)
	}
}

func TestBuildQEMUArgsAppendsExtraArgs(t *testing.T) {
	cfg := normalizedTestConfig(t, ArchAMD64)
	cfg.ExtraQEMUArgs = []string{"-device", "virtio-rng-pci"}

	args, err := BuildQEMUArgs(cfg, 3, Firmware{})
	if err != nil {
		t.Fatalf("BuildQEMUArgs() error = %v", err)
	}

	if len(args) < 2 || args[len(args)-2] != "-device" || args[len(args)-1] != "virtio-rng-pci" {
		t.Fatalf("extra args were not appended: %v", args)
	}
}

func normalizedTestConfig(t *testing.T, arch string) Config {
	t.Helper()

	cfg, err := Normalize(Config{
		Name:        "testvm",
		Arch:        arch,
		CPUs:        4,
		Memory:      "4096M",
		VXLANRemote: "100.64.0.8",
		MAC:         "02:00:00:00:00:01",
	})
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}

	return cfg
}

func containsArg(args []string, value string) bool {
	return countArg(args, value) > 0
}

func countArg(args []string, value string) int {
	count := 0

	for _, arg := range args {
		if arg == value {
			count++
		}
	}

	return count
}
