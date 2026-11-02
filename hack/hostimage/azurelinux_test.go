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
		"AZURELINUX_KERNEL_HWE_DRIVERS_GPU_VERSION=6.18.48.1-1.azl3",
		"AZURELINUX_GRUB_VERSION=2.06-27.azl3",
		"AZURELINUX_CUDA_OPEN_HWE_VERSION=580.159.04-2_6.18.48.1.1.azl3",
		"AZURELINUX_MLNX_OFA_KERNEL_VERSION=26.01-1_6.6.158.1.1.azl3",
		"AZURELINUX_MLNX_OFA_KERNEL_HWE_DEVEL_VERSION=26.01-2_6.18.48.1.1.azl3",
		"AZURELINUX_MLNX_OFA_KERNEL_HWE_MODULES_VERSION=26.01-2_6.18.48.1.1.azl3",
		"AZURELINUX_OCL_ICD_VERSION=2.2.13-2.azl3",
		"AZURELINUX_OPENCL_FILESYSTEM_VERSION=1.0-13.azl3",
		"AZURELINUX_BINUTILS_VERSION=2.41-13.azl3",
		"AZURELINUX_ELFUTILS_LIBELF_DEVEL_VERSION=0.189-6.azl3",
		"AZURELINUX_GCC_VERSION=13.2.0-7.azl3",
		"AZURELINUX_MAKE_VERSION=4.4.1-2.azl3",
		"AZURELINUX_PATCH_VERSION=2.7.6-10.azl3",
		"NVIDIA_DRIVER_VERSION=595.91.07",
		"NVIDIA_DRIVER_RPM_VERSION=595.91.07-1.azl3",
		"NVIDIA_DKMS_RPM_VERSION=3.4.1-1.azl3",
		"NVIDIA_LOCAL_REPO_SHA256=e49c4eab0f07f9737897eb7595fef9200d65a038f4061965f3cfcf664fd82407",
		"NVIDIA_LOCAL_REPO_KEY_FILE=8806481F.pub",
		"NVIDIA_LOCAL_REPO_KEY_SHA256=d3139840cd5ba220eefc2110470521767e928a0f3b793551435236c74b186781",
		"NVIDIA_FABRIC_PROBE_SHA256=6370ab719a3bb318fb865afff79c0eb67d3d23f094029379c9321250486d24c7",
	} {
		require.Contains(t, versions, expected)
	}

	require.NotContains(t, versions, "latest")

	containerfile := readRepoFile(t, "images/host-azurelinux3-nvidia/Containerfile")
	require.Contains(t, containerfile, "versions.env")
	require.Contains(t, containerfile, "ARG IMAGE_VARIANT=nvidia")
	require.Contains(t, containerfile, "ARG NVIDIA_DRIVER_SOURCE=nvidia-localrepo")
	require.Contains(t, containerfile, `IMAGE_VARIANT="${IMAGE_VARIANT}"`)
	require.Contains(t, containerfile, `NVIDIA_DRIVER_SOURCE="${NVIDIA_DRIVER_SOURCE}"`)
	require.NotContains(t, containerfile, "host-ubuntu")

	rootfs := readRepoFile(t, "images/host-azurelinux3/assets/rootfs.sh")
	require.Contains(t, rootfs, `[ "$NVIDIA" = 1 ] && [ "$TARGETARCH" = arm64 ]`)
	require.Contains(t, rootfs, `KERNEL_PACKAGE="${KERNEL_PACKAGE:-kernel}"`)
	require.Contains(t, rootfs, `retry tdnf install -y "$KERNEL_PACKAGE"`)
	require.Contains(t, rootfs, "retry tdnf install -y grub2-efi-binary cuda-open-hwe")
	require.Contains(t, rootfs, "inconsistent image variant and NVIDIA setting")
	require.Contains(t, rootfs, "unsupported NVIDIA driver source")
	require.Contains(t, rootfs, "boot_profile=nvidia-hwe")
}

func TestAzureLinuxNVIDIALocalRepoBuildFailsClosed(t *testing.T) {
	rootfs := readRepoFile(t, "images/host-azurelinux3/assets/rootfs.sh")
	for _, expected := range []string{
		`verify_file_sha256 "$repo_rpm" "$NVIDIA_LOCAL_REPO_SHA256"`,
		`repo_key="$repo_dir/$NVIDIA_LOCAL_REPO_KEY_FILE"`,
		`verify_file_sha256 "$repo_key" "$NVIDIA_LOCAL_REPO_KEY_SHA256"`,
		`rpm --install --nodeps --nosignature "$repo_rpm"`,
		`for package in "$repo_rpm" "$repo_dir"/*.rpm`,
		`rpm --checksig "$package"`,
		`verify_file_sha256 "$fabric_probe" "$NVIDIA_FABRIC_PROBE_SHA256"`,
		`rpm --install --nodeps --noscripts "$kmod_rpm"`,
		`dkms add -m nvidia -v "$NVIDIA_DRIVER_VERSION" --rpm_safe_upgrade`,
		`dkms build -m nvidia -v "$NVIDIA_DRIVER_VERSION"`,
		`-k "$NVIDIA_KVER" --force`,
		`cat "/var/lib/dkms/nvidia/$NVIDIA_DRIVER_VERSION/build/make.log"`,
		`dkms install -m nvidia -v "$NVIDIA_DRIVER_VERSION"`,
		`NVIDIA DKMS did not install for $NVIDIA_KVER`,
		`for module in nvidia nvidia-modeset nvidia-drm nvidia-uvm nvidia-peermem`,
		`"$module vermagic is $vermagic, expected kernel $NVIDIA_KVER"`,
		`disable_nvidia_vendor_service nvidia-persistenced.service`,
		`disable_nvidia_vendor_service nvidia-powerd.service`,
		`systemctl mask nvidia-persistence-mode.service`,
		`ExecStart=/usr/bin/nvidia-smi -pm 0`,
		`"$unit enablement is $state, expected disabled or not-found"`,
		`"$unit is linked into a boot target"`,
		`NVIDIA driver A/B image unexpectedly contains another driver or fabric component`,
		`kernel-devel-${AZURELINUX_KERNEL_VERSION}`,
		`kernel-hwe-drivers-gpu-${AZURELINUX_KERNEL_HWE_DRIVERS_GPU_VERSION}`,
		`mlnx-ofa_kernel-hwe-devel-${AZURELINUX_MLNX_OFA_KERNEL_HWE_DEVEL_VERSION}`,
		`mlnx-ofa_kernel-hwe-modules-${AZURELINUX_MLNX_OFA_KERNEL_HWE_MODULES_VERSION}`,
		`baseurl=https://packages.microsoft.com/azurelinux/$releasever/prod/extended/$basearch`,
		`ocl-icd-${AZURELINUX_OCL_ICD_VERSION}`,
		`opencl-filesystem-${AZURELINUX_OPENCL_FILESYSTEM_VERSION}`,
		`binutils-${AZURELINUX_BINUTILS_VERSION}`,
		`elfutils-libelf-devel-${AZURELINUX_ELFUTILS_LIBELF_DEVEL_VERSION}`,
		`gcc-${AZURELINUX_GCC_VERSION}`,
		`gcc-c++-${AZURELINUX_GCC_VERSION}`,
		`make-${AZURELINUX_MAKE_VERSION}`,
		`patch-${AZURELINUX_PATCH_VERSION}`,
		`for command in as gcc g++ ld make patch`,
		`NVIDIA DKMS toolchain command is missing: $command`,
		`modprobe --show-depends`,
		`nvidia_drm dependency is missing from $NVIDIA_KVER`,
	} {
		require.Contains(t, rootfs, expected)
	}

	require.Less(
		t,
		strings.Index(rootfs, `"mlnx-ofa_kernel-hwe-modules-${AZURELINUX_MLNX_OFA_KERNEL_HWE_MODULES_VERSION}"`),
		strings.Index(rootfs, `rpm --install --nodeps --noscripts "$kmod_rpm"`),
		"OFED must be installed before the NVIDIA driver",
	)
	require.Less(
		t,
		strings.Index(rootfs, `"kernel-hwe-drivers-gpu-${AZURELINUX_KERNEL_HWE_DRIVERS_GPU_VERSION}"`),
		strings.Index(rootfs, `rpm --install --nodeps --noscripts "$kmod_rpm"`),
		"NVIDIA DRM dependencies must be installed before the NVIDIA driver",
	)

	for _, forbidden := range []string{
		`tdnf install -y nvidia-imex`,
		`tdnf install -y nvidia-fabricmanager`,
		`tdnf install -y nvlink5`,
	} {
		require.NotContains(t, rootfs, forbidden)
	}
}

func TestAzureLinuxNVIDIAGRUBConfigDefaultsToHWEWithoutGraceArgs(t *testing.T) {
	output := filepath.Join(t.TempDir(), "grub.cfg")
	script := filepath.Join(repoRoot(t), "images/host-azurelinux3/assets/grub-config.sh")
	command := exec.Command("sh", script)

	command.Env = append(
		os.Environ(),
		"OUTPUT="+output,
		"ROOT_UUID=test-root",
		"STANDARD_KVER=6.6.157.1-1.azl3",
		"NVIDIA_KVER=6.18.48.1-1.azl3",
		"BOOT_PROFILE=nvidia-hwe",
	)
	result, err := command.CombinedOutput()
	require.NoError(t, err, string(result))

	config, err := os.ReadFile(output)
	require.NoError(t, err)

	grub := string(config)
	for _, expected := range []string{
		"set default=nvidia-grace",
		"linux /boot/vmlinuz-6.18.48.1-1.azl3 root=UUID=test-root rw ",
		"linux /boot/vmlinuz-6.6.157.1-1.azl3 root=UUID=test-root rw ",
	} {
		require.Contains(t, grub, expected)
	}

	require.NotContains(t, grub, "smbios")
	require.NotContains(t, grub, "cpu_manufacturer")
	require.NotContains(t, grub, "$nvidia_args")
	require.NotContains(t, grub, "iommu.passthrough")
	require.NotContains(t, grub, "irqchip.gicv3_nolpi")
	require.NotContains(t, grub, "arm_smmu_v3.disable_msipolling")
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
		"BOOT_PROFILE=standard",
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

func TestAzureLinuxNVIDIAGRUBConfigRequiresHWEKernel(t *testing.T) {
	output := filepath.Join(t.TempDir(), "grub.cfg")
	script := filepath.Join(repoRoot(t), "images/host-azurelinux3/assets/grub-config.sh")
	command := exec.Command("sh", script)

	command.Env = append(
		os.Environ(),
		"OUTPUT="+output,
		"ROOT_UUID=test-root",
		"STANDARD_KVER=6.6.157.1-1.azl3",
		"BOOT_PROFILE=nvidia-hwe",
	)
	result, err := command.CombinedOutput()
	require.Error(t, err)
	require.Contains(t, string(result), "nvidia-hwe boot profile requires NVIDIA_KVER")
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
	require.NotContains(t, output, "iommu.passthrough")
	require.NotContains(t, output, "irqchip.gicv3_nolpi")
	require.NotContains(t, output, "arm_smmu_v3.disable_msipolling")
	require.NotContains(t, output, "smbios")
	require.NotContains(t, output, "cpu_manufacturer")
}

func TestAzureLinuxImageBuildChecksFailClosed(t *testing.T) {
	rootfs := readRepoFile(t, "images/host-azurelinux3/assets/rootfs.sh")
	for _, expected := range []string{
		"both kernel and kernel-hwe must provide a bootable kernel",
		"GRUB module /usr/lib/grub/arm64-efi/smbios.mod",
		"cuda-open-hwe resolved to",
		"cuda-open-hwe is not matched to kernel-hwe",
		"NVIDIA DKMS did not install for",
		"NVIDIA local repository payload is incomplete",
		"/etc/modprobe.d/blacklist-nouveau.conf",
		"nvidia-device-setup.service",
		"legacy_persistence_mode=off",
	} {
		require.True(t, strings.Contains(rootfs, expected), "missing fail-closed check %q", expected)
	}

	disk := readRepoFile(t, "images/host-azurelinux3/assets/disk.sh")
	require.Contains(t, disk, `mkdir -p /disk`)
	require.Contains(t, disk, `> /disk/disk.img.gz`)
	require.Contains(t, disk, "required GRUB smbios.mod is missing")
	require.Contains(t, disk, "rootfs image variant")
	require.Contains(t, disk, "NVIDIA ARM64 image requires kernel-hwe")
	require.Contains(t, disk, "NVIDIA HWE image unexpectedly contains Grace argument")
	require.Contains(t, disk, "standard image unexpectedly contains NVIDIA Grace arguments")

	verify := readRepoFile(t, "images/host-azurelinux3/assets/verify-disk.sh")
	require.Contains(t, verify, `STANDARD_ENTRY=$(printf '%s\n' "$GRUB"`)
	require.Contains(t, verify, "ordinary ARM64 entry unexpectedly contains Grace arguments")
	require.Contains(t, verify, "NVIDIA Grace entry does not select kernel-hwe")
	require.Contains(t, verify, "NVIDIA image unexpectedly uses runtime SMBIOS selection")
	require.Contains(t, verify, `grep -q '^Inode:'`)
	require.Contains(t, verify, "PASS package inputs:")
	require.Contains(t, verify, "PASS NVIDIA 595.91.07 target-kernel DKMS modules:")
	require.Contains(t, verify, "PASS NVIDIA DRM dependency closure:")
	require.Contains(t, verify, "kernel-hwe-drivers-gpu provides ttm and drm_ttm_helper")
	require.Contains(t, verify, "GSP firmware=595.91.07")
	require.Contains(t, verify, "failed to enumerate regular systemd unit files")
	require.Contains(t, verify, "failed to scan regular systemd unit file")
	require.Contains(t, verify, `done < "$unit_list"`)
	require.Contains(t, verify, "PASS idle-service baseline:")
	require.Contains(t, verify, "nvidia-persistenced.service is disabled or absent")
	require.Contains(t, verify, "nvidia-powerd.service is disabled or absent")
	require.Contains(t, verify, "nvidia-persistence-mode.service is masked")
	require.Contains(t, verify, "legacy NVIDIA persistence mode is unexpectedly enabled by systemd")
	require.Contains(t, verify, "ExecStart=/usr/bin/nvidia-smi -pm 0")
	require.Contains(t, verify, "$WORK/systemd-etc/system/nvidia-persistence-mode.service")
	require.Contains(t, verify, "$WORK/systemd-etc/system/multi-user.target.wants/nvidia-device-setup.service")
	require.Contains(t, verify, "PASS specialized NVIDIA GRUB policy:")
	require.Contains(t, verify, "PASS MANA-compatible kernel command line:")
	require.Contains(t, verify, "GICv3 LPI/ITS MSI-X remains enabled")

	workflow := readRepoFile(t, ".github/workflows/host-azurelinux3-nvidia.yaml")
	require.Contains(t, workflow, "timeout-minutes: 120")
	require.Contains(t, workflow, "runs-on: ubuntu-24.04-arm")
	require.NotContains(t, workflow, "docker/setup-qemu-action")
	require.Contains(t, workflow, "- .github/workflows/images.yaml")

	publishWorkflow := readRepoFile(t, ".github/workflows/images.yaml")
	require.Contains(
		t,
		publishWorkflow,
		"needs.resolve.outputs.platforms == 'linux/arm64' && 'ubuntu-24.04-arm' || 'ubuntu-latest'",
	)
	require.Contains(t, publishWorkflow, "if: needs.resolve.outputs.platforms != 'linux/arm64'")
}

func TestAzureLinuxNVIDIAARM64DisablesLegacyPersistenceMode(t *testing.T) {
	rootfs := readRepoFile(t, "images/host-azurelinux3/assets/rootfs.sh")
	start := strings.Index(rootfs, `    # Create the device nodes needed by bootstrap`)
	require.NotEqual(t, -1, start)
	end := strings.Index(rootfs[start:], `  if [ "$TARGETARCH" = arm64 ]; then
    mkdir -p /etc/unbounded`)
	require.NotEqual(t, -1, end)
	servicePolicy := rootfs[start : start+end]
	amd64Start := strings.Index(servicePolicy, "  else")
	require.NotEqual(t, -1, amd64Start)
	arm64Policy := servicePolicy[:amd64Start]
	amd64Policy := servicePolicy[amd64Start:]

	require.Contains(t, arm64Policy, "nvidia-device-setup.service")
	require.Contains(t, arm64Policy, "ExecStart=/usr/bin/nvidia-smi -pm 0")
	require.Contains(t, arm64Policy, "systemctl mask nvidia-persistence-mode.service")
	require.NotContains(t, arm64Policy, "RemainAfterExit=yes")
	require.Contains(t, amd64Policy, "nvidia-persistence-mode.service")
	require.Contains(t, amd64Policy, "RemainAfterExit=yes")
	require.Contains(t, amd64Policy, "ExecStart=/usr/bin/nvidia-smi -pm 1")
}
