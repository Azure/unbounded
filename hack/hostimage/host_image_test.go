// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package hostimage

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

func repoRoot(t *testing.T) string {
	t.Helper()

	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok)

	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func readRepoFile(t *testing.T, path string) string {
	t.Helper()

	data, err := os.ReadFile(filepath.Join(repoRoot(t), path))
	require.NoError(t, err)

	return string(data)
}

func requireContainsAll(t *testing.T, contents string, values ...string) {
	t.Helper()

	for _, value := range values {
		require.Contains(t, contents, value)
	}
}

func TestUbuntuGB200ImageContract(t *testing.T) {
	containerfile := readRepoFile(t, "images/host-ubuntu2404-nvidia/Containerfile")
	rootfs := readRepoFile(t, "images/host-ubuntu2404-nvidia/assets/rootfs.sh")
	disk := readRepoFile(t, "images/host-ubuntu2404-nvidia/assets/disk.sh")

	requireContainsAll(
		t, containerfile,
		"FROM --platform=linux/arm64",
		"ARG KERNEL_RELEASE=6.8.0-1039-azure-nvidia",
		"ARG NVIDIA_KERNEL_MODULE_VERSION=6.8.0-1039.42+3",
		"ARG NVIDIA_DRIVER_VERSION=580.178.04-0ubuntu0.24.04.1",
		"ARG NVIDIA_TOOLKIT_VERSION=1.19.1-1",
		"ARG NVIDIA_IMEX_VERSION=580.178.04-1ubuntu1",
		"COPY --from=build /disk /disk",
	)
	requireContainsAll(
		t, rootfs,
		"host-ubuntu2404-nvidia only supports arm64",
		"linux-modules-nvidia-580-server-open-${KERNEL_RELEASE}",
		"nvidia-fabricmanager-580=${NVIDIA_DRIVER_VERSION}",
		"libnvidia-nscq-580=${NVIDIA_DRIVER_VERSION}",
		"nvidia-container-toolkit=${NVIDIA_TOOLKIT_VERSION}",
		"nvidia-imex=${NVIDIA_IMEX_VERSION}",
		"nvidia-modprobe=${NVIDIA_IMEX_VERSION}",
		"CUDA_KEY_FINGERPRINT=EB693B3035CD5710E231E123A4B469963BF863CC",
		"IMEX_SHA256=01d1bef149c56c1dc597b42baf92193241ff1d155f53d59c8d1de8ac968365b6",
		"MODPROBE_SHA256=62e1398d13663c2c0e4a75d5391ac1e3984f9c3369230260a8e92e2ad5df4e59",
		"sha256sum --check --strict",
		"CONFIG_MICROSOFT_MANA=y",
		"name 'mana_ib.ko*'",
		"rdma-core ibverbs-providers",
		"systemctl mask nvidia-imex.service",
	)
	require.NotContains(t, rootfs, "|| true\nretry apt-get install")
	requireContainsAll(
		t, disk,
		"label: gpt",
		"type=C12A7328-F81F-11D2-BA4B-00A0C93EC93B",
		"::/EFI/BOOT/BOOTAA64.EFI",
		"pigz -1 -c \"$DISK\" > /disk/disk.img.gz",
	)
}

func TestUbuntuGB200ReleaseSurfacesAreArm64Only(t *testing.T) {
	releaseWorkflow := readRepoFile(t, ".github/workflows/release.yaml")
	imageWorkflow := readRepoFile(t, ".github/workflows/images.yaml")
	buildWorkflow := readRepoFile(t, ".github/workflows/host-ubuntu2404-nvidia.yaml")
	releaseBOM := readRepoFile(t, "hack/cmd/release-bom/main.go")

	for _, line := range []string{
		"- name: host-ubuntu2404-nvidia",
		"file: images/host-ubuntu2404-nvidia/Containerfile",
		"platforms: linux/arm64",
	} {
		require.Contains(t, releaseWorkflow, line)
	}

	require.Contains(t, imageWorkflow, `"$name" == "host-ubuntu2404-nvidia"`)
	require.Contains(t, imageWorkflow, `platforms="linux/arm64"`)
	require.Contains(t, buildWorkflow, "runs-on: ubuntu-24.04-arm")
	require.Contains(t, buildWorkflow, "platforms: linux/arm64")
	require.Contains(t, buildWorkflow, "hack/verify-host-ubuntu2404-nvidia.sh tmp/host-ubuntu2404-nvidia")
	require.Contains(t, releaseBOM, `"host-ubuntu2404-nvidia"`)
}
