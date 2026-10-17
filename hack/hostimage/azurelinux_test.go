// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package hostimage

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func repoRoot(t *testing.T) string {
	t.Helper()

	root, err := filepath.Abs(filepath.Join("..", ".."))
	require.NoError(t, err)

	return root
}

func readRepoFile(t *testing.T, path string) string {
	t.Helper()

	content, err := os.ReadFile(filepath.Join(repoRoot(t), path))
	require.NoError(t, err)

	return string(content)
}

func TestAzureLinuxNVIDIAInputsArePinned(t *testing.T) {
	versions := readRepoFile(t, "images/host-azurelinux3-nvidia/versions.env")
	for _, expected := range []string{
		"AZURELINUX_KERNEL_VERSION=6.6.157.1-1.azl3",
		"AZURELINUX_KERNEL_HWE_VERSION=6.18.48.1-1.azl3",
		"AZURELINUX_GRUB_VERSION=2.06-27.azl3",
		"AZURELINUX_CUDA_OPEN_HWE_VERSION=580.159.04-2_6.18.48.1.1.azl3",
	} {
		require.Contains(t, versions, expected)
	}

	require.NotContains(t, versions, "latest")

	containerfile := readRepoFile(t, "images/host-azurelinux3-nvidia/Containerfile")
	require.Contains(t, containerfile, "versions.env")
	require.NotContains(t, containerfile, "host-ubuntu")

	rootfs := readRepoFile(t, "images/host-azurelinux3/assets/rootfs.sh")
	require.Contains(t, rootfs, `[ "$NVIDIA" = 1 ] && [ "$TARGETARCH" = arm64 ]`)
	require.Contains(t, rootfs, `KERNEL_PACKAGE="${KERNEL_PACKAGE:-kernel}"`)
	require.Contains(t, rootfs, `retry tdnf install -y "$KERNEL_PACKAGE"`)
	require.Contains(t, rootfs, "retry tdnf install -y grub2-efi-binary cuda-open-hwe")
}

func TestAzureLinuxGraceGRUBConfig(t *testing.T) {
	output := filepath.Join(t.TempDir(), "grub.cfg")
	script := filepath.Join(repoRoot(t), "images/host-azurelinux3/assets/grub-config.sh")
	command := exec.Command("sh", script)

	command.Env = append(
		os.Environ(),
		"OUTPUT="+output,
		"ROOT_UUID=test-root",
		"STANDARD_KVER=6.6.157.1-1.azl3",
		"NVIDIA_KVER=6.18.48.1-1.azl3",
	)
	result, err := command.CombinedOutput()
	require.NoError(t, err, string(result))

	config, err := os.ReadFile(output)
	require.NoError(t, err)

	grub := string(config)
	for _, expected := range []string{
		"insmod smbios",
		"smbios --type 4 --get-string 7 --set cpu_manufacturer",
		"if [ x$cpu_manufacturer = xNVIDIA ]; then",
		"set default=nvidia-grace",
		"set default=standard",
		"iommu.passthrough=1 irqchip.gicv3_nolpi=y arm_smmu_v3.disable_msipolling=1",
		"linux /boot/vmlinuz-6.18.48.1-1.azl3 root=UUID=test-root rw  $nvidia_args",
		"linux /boot/vmlinuz-6.6.157.1-1.azl3 root=UUID=test-root rw ",
	} {
		require.Contains(t, grub, expected)
	}
}

func TestAzureLinuxStandardGRUBConfigHasNoGraceBehavior(t *testing.T) {
	output := filepath.Join(t.TempDir(), "grub.cfg")
	script := filepath.Join(repoRoot(t), "images/host-azurelinux3/assets/grub-config.sh")
	command := exec.Command("sh", script)

	command.Env = append(
		os.Environ(),
		"OUTPUT="+output,
		"ROOT_UUID=test-root",
		"STANDARD_KVER=6.6.157.1-1.azl3",
	)
	result, err := command.CombinedOutput()
	require.NoError(t, err, string(result))

	config, err := os.ReadFile(output)
	require.NoError(t, err)

	grub := string(config)
	require.Contains(t, grub, "linux /boot/vmlinuz-6.6.157.1-1.azl3")
	require.NotContains(t, grub, "smbios")
	require.NotContains(t, grub, "nvidia_args")
	require.NotContains(t, grub, "iommu.passthrough")
}

func TestAzureLinuxGRUBSelectorUsesRPMOwnership(t *testing.T) {
	dir := t.TempDir()
	boot := filepath.Join(dir, "boot")
	require.NoError(t, os.MkdirAll(boot, 0o755))

	for _, kernel := range []string{"6.6.157.1-1.azl3", "6.18.48.1-1.azl3"} {
		require.NoError(t, os.WriteFile(filepath.Join(boot, "vmlinuz-"+kernel), nil, 0o644))
	}

	grubLinux := filepath.Join(dir, "10_linux")
	require.NoError(t, os.WriteFile(grubLinux, []byte(`boot_device_id=test-root
grub_probe=module_probe
module_probe() { printf '%s\n' /dev/root; }
is_path_readable_by_grub() { return 0; }
prepare_grub_to_access_device() { printf '%s\n' 'search --fs-uuid test-root'; }
make_system_path_relative_to_its_root() { printf '%s\n' /lib/grub; }
`), 0o644))

	rpm := filepath.Join(dir, "rpm")
	require.NoError(t, os.WriteFile(rpm, []byte(`#!/bin/sh
case "${4##*/}" in
  vmlinuz-6.6.157.1-1.azl3) printf kernel ;;
  vmlinuz-6.18.48.1-1.azl3) printf kernel-hwe ;;
  *) exit 1 ;;
esac
`), 0o755))

	script := filepath.Join(repoRoot(t), "images/host-azurelinux3/assets/10_azure_nvidia")
	command := exec.Command("sh", script)

	command.Env = append(
		os.Environ(),
		"PATH="+dir+":"+os.Getenv("PATH"),
		"BOOT_DIR="+boot,
		"GRUB_LINUX_SCRIPT="+grubLinux,
	)
	result, err := command.CombinedOutput()
	require.NoError(t, err, string(result))

	output := string(result)
	require.Contains(t, output, `set default="gnulinux-6.18.48.1-1.azl3-advanced-test-root"`)
	require.Contains(t, output, `set default="gnulinux-6.6.157.1-1.azl3-advanced-test-root"`)
	require.Contains(t, output, "insmod smbios")
	require.Contains(t, output, "iommu.passthrough=1 irqchip.gicv3_nolpi=y arm_smmu_v3.disable_msipolling=1")
}

func TestAzureLinuxImageBuildChecksFailClosed(t *testing.T) {
	rootfs := readRepoFile(t, "images/host-azurelinux3/assets/rootfs.sh")
	for _, expected := range []string{
		"both kernel and kernel-hwe must provide a bootable kernel",
		"GRUB module /usr/lib/grub/arm64-efi/smbios.mod",
		"cuda-open-hwe resolved to",
		"cuda-open-hwe is not matched to kernel-hwe",
		"/etc/modprobe.d/blacklist-nouveau.conf",
		"nvidia-persistence-mode.service",
	} {
		require.True(t, strings.Contains(rootfs, expected), "missing fail-closed check %q", expected)
	}

	disk := readRepoFile(t, "images/host-azurelinux3/assets/disk.sh")
	require.Contains(t, disk, `mkdir -p /disk`)
	require.Contains(t, disk, `> /disk/disk.img.gz`)
	require.Contains(t, disk, "required GRUB smbios.mod is missing")

	verify := readRepoFile(t, "images/host-azurelinux3/assets/verify-disk.sh")
	require.Contains(t, verify, `STANDARD_ENTRY=$(printf '%s\n' "$GRUB"`)
	require.Contains(t, verify, "ordinary ARM64 entry unexpectedly contains Grace arguments")
	require.Contains(t, verify, "NVIDIA Grace entry does not select kernel-hwe")
	require.Contains(t, verify, "PASS package inputs:")
	require.Contains(t, verify, "PASS GRUB hardware selection:")
}
