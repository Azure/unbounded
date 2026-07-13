// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package goalstates

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDiscoverNVIDIADevicesIncludesIMEXChannels(t *testing.T) {
	t.Parallel()

	deviceDir := t.TempDir()
	imexDir := filepath.Join(deviceDir, nvidiaIMEXDirName)
	channel := filepath.Join(imexDir, "channel0")
	require.NoError(t, os.MkdirAll(imexDir, 0o755))
	require.NoError(t, os.WriteFile(channel, nil, 0o644))

	require.Contains(t, discoverNVIDIADevicesIn(deviceDir), channel)
}

func TestParseNVIDIALibraries(t *testing.T) {
	ldconfigOutput := []byte(`	linux-vdso.so.1 (LINUX_VDSO) => linux-vdso.so.1
	libcuda.so.1 (libc6,x86-64) => /usr/lib/x86_64-linux-gnu/libcuda.so.1
	libcuda.so.1 (libc6) => /usr/lib/i386-linux-gnu/libcuda.so.1
	libnvidia-ml.so.1 (libc6,x86-64) => /usr/lib/x86_64-linux-gnu/libnvidia-ml.so.1
	libnvidia-ml.so.580.126.09 (libc6,x86-64) => /usr/lib/x86_64-linux-gnu/libnvidia-ml.so.580.126.09
	libEGL_nvidia.so.0 (libc6,x86-64) => /usr/lib/x86_64-linux-gnu/libEGL_nvidia.so.0
	libGLX_nvidia.so.0 (libc6,x86-64) => /usr/lib/x86_64-linux-gnu/libGLX_nvidia.so.0
	libGLESv2_nvidia.so.2 (libc6,x86-64) => /usr/lib/x86_64-linux-gnu/libGLESv2_nvidia.so.2
	libnvcuvid.so.1 (libc6,x86-64) => /usr/lib/x86_64-linux-gnu/libnvcuvid.so.1
	libnvoptix.so.1 (libc6,x86-64) => /usr/lib/x86_64-linux-gnu/libnvoptix.so.1
	libpthread.so.0 (libc6,x86-64) => /lib/x86_64-linux-gnu/libpthread.so.0
`)

	libs := parseNVIDIALibraries(ldconfigOutput, "x86-64")

	// Should have 8 NVIDIA libs (the 32-bit libcuda should be filtered out,
	// and non-NVIDIA libs like libpthread should be excluded).
	if got := len(libs); got != 8 {
		t.Fatalf("parseNVIDIALibraries() returned %d libs, want 8", got)
	}

	// Verify first entry.
	if libs[0].HostPath != "/usr/lib/x86_64-linux-gnu/libcuda.so.1" {
		t.Errorf("libs[0].HostPath = %q, want %q", libs[0].HostPath, "/usr/lib/x86_64-linux-gnu/libcuda.so.1")
	}

	// Verify no 32-bit libs.
	for _, lib := range libs {
		if lib.HostPath == "/usr/lib/i386-linux-gnu/libcuda.so.1" {
			t.Errorf("32-bit libcuda should have been filtered out")
		}
	}

	// Verify non-NVIDIA libs are excluded.
	for _, lib := range libs {
		if lib.HostPath == "/lib/x86_64-linux-gnu/libpthread.so.0" {
			t.Errorf("non-NVIDIA library libpthread should have been excluded")
		}
	}
}

func TestExpandNVIDIALibrariesFindsVersionedTargetsMissingFromLDConfig(t *testing.T) {
	t.Parallel()

	libDir := t.TempDir()
	version := "580.167.08"

	for _, family := range []string{"libcuda.so", "libnvidia-ml.so"} {
		versionedName := family + "." + version
		require.NoError(t, os.WriteFile(filepath.Join(libDir, versionedName), []byte(family), 0o644))
		require.NoError(t, os.Symlink(versionedName, filepath.Join(libDir, family+".1")))
	}

	require.NoError(t, os.Symlink("libcuda.so.1", filepath.Join(libDir, "libcuda.so")))
	require.NoError(t, os.WriteFile(filepath.Join(libDir, "libcudadebugger.so."+version), []byte("debugger"), 0o644))

	ldconfigOutput := []byte(
		"libcuda.so.1 (libc6,x86-64) => " + filepath.Join(libDir, "libcuda.so.1") + "\n" +
			"libcuda.so (libc6,x86-64) => " + filepath.Join(libDir, "libcuda.so") + "\n" +
			"libnvidia-ml.so.1 (libc6,x86-64) => " + filepath.Join(libDir, "libnvidia-ml.so.1") + "\n",
	)

	libs := expandNVIDIALibraries(parseNVIDIALibraries(ldconfigOutput, "x86-64"))

	paths := make([]string, 0, len(libs))
	for _, lib := range libs {
		paths = append(paths, lib.HostPath)
	}

	require.Contains(t, paths, filepath.Join(libDir, "libcuda.so."+version))
	require.Contains(t, paths, filepath.Join(libDir, "libcudadebugger.so."+version))
	require.Contains(t, paths, filepath.Join(libDir, "libnvidia-ml.so."+version))
	require.Equal(t, version, discoverNVIDIADriverVersion(libs, filepath.Join(t.TempDir(), "missing-version")))
}

func TestExpandNVIDIALibrariesUsesSameBasenameSymlinkTargetAsSource(t *testing.T) {
	t.Parallel()

	aliasDir := t.TempDir()
	targetDir := t.TempDir()
	aliasPath := filepath.Join(aliasDir, "libcuda.so.1")
	targetPath := filepath.Join(targetDir, "libcuda.so.1")

	require.NoError(t, os.WriteFile(targetPath, []byte("cuda"), 0o644))
	require.NoError(t, os.Symlink(targetPath, aliasPath))

	libs := expandNVIDIALibraries([]NvidiaLibMapping{{HostPath: aliasPath}})
	require.Len(t, libs, 1)
	require.Equal(t, targetPath, libs[0].ResolvedHostPath)

	libs, mounts := buildNVIDIALibMounts(libs, "/usr/lib/x86_64-linux-gnu")
	require.Len(t, mounts, 1)
	require.Equal(t, targetDir, mounts[0].HostDir)
	require.Equal(t, "/run/host-nvidia/0/libcuda.so.1", libs[0].ContainerPath)
	require.Equal(t, "/usr/lib/x86_64-linux-gnu/libcuda.so.1", libs[0].LinkPath)
}

func TestDiscoverNVIDIADriverVersionFallsBackToGLX(t *testing.T) {
	t.Parallel()

	libs := []NvidiaLibMapping{{HostPath: "/usr/lib/x86_64-linux-gnu/libGLX_nvidia.so.580.167.08"}}
	require.Equal(t, "580.167.08", discoverNVIDIADriverVersion(libs, filepath.Join(t.TempDir(), "missing-version")))
}

func TestDiscoverNVIDIADriverVersionFallsBackToKernelModule(t *testing.T) {
	t.Parallel()

	libDir := t.TempDir()
	moduleVersionPath := filepath.Join(t.TempDir(), "version")
	require.NoError(t, os.WriteFile(filepath.Join(libDir, "libcuda.so.1"), []byte("driver"), 0o644))
	require.NoError(t, os.WriteFile(moduleVersionPath, []byte("580.167.08\n"), 0o644))

	libs := []NvidiaLibMapping{{HostPath: filepath.Join(libDir, "libcuda.so.1")}}
	require.Equal(t, "580.167.08", discoverNVIDIADriverVersion(libs, moduleVersionPath))
}

func TestParseNVIDIALibraries_ARM64(t *testing.T) {
	ldconfigOutput := []byte(`	linux-vdso.so.1 (LINUX_VDSO) => linux-vdso.so.1
	libcuda.so.1 (libc6,aarch64) => /usr/lib/aarch64-linux-gnu/libcuda.so.1
	libcuda.so.1 (libc6,x86-64) => /usr/lib/x86_64-linux-gnu/libcuda.so.1
	libnvidia-ml.so.1 (libc6,aarch64) => /usr/lib/aarch64-linux-gnu/libnvidia-ml.so.1
	libpthread.so.0 (libc6,aarch64) => /lib/aarch64-linux-gnu/libpthread.so.0
`)

	libs := parseNVIDIALibraries(ldconfigOutput, "aarch64")

	// Should have 2 NVIDIA libs: only aarch64 entries, no x86-64 or non-NVIDIA.
	if got := len(libs); got != 2 {
		t.Fatalf("parseNVIDIALibraries() returned %d libs, want 2", got)
	}

	if libs[0].HostPath != "/usr/lib/aarch64-linux-gnu/libcuda.so.1" {
		t.Errorf("libs[0].HostPath = %q, want aarch64 path", libs[0].HostPath)
	}

	// Verify x86-64 entry is excluded.
	for _, lib := range libs {
		if lib.HostPath == "/usr/lib/x86_64-linux-gnu/libcuda.so.1" {
			t.Errorf("x86-64 libcuda should have been filtered out for aarch64 arch tag")
		}
	}
}

func TestParseNVIDIALibraries_Empty(t *testing.T) {
	libs := parseNVIDIALibraries([]byte(""), "x86-64")
	if len(libs) != 0 {
		t.Errorf("parseNVIDIALibraries(empty) returned %d libs, want 0", len(libs))
	}

	libs = parseNVIDIALibraries([]byte("some garbage output\nno libraries here"), "x86-64")
	if len(libs) != 0 {
		t.Errorf("parseNVIDIALibraries(garbage) returned %d libs, want 0", len(libs))
	}
}

func TestParseNVIDIALibraries_DeduplicatesByBasename(t *testing.T) {
	ldconfigOutput := []byte(`	libcuda.so.1 (libc6,x86-64) => /usr/lib/x86_64-linux-gnu/libcuda.so.1
	libcuda.so.1 (libc6,x86-64) => /usr/lib64/libcuda.so.1
`)

	libs := parseNVIDIALibraries(ldconfigOutput, "x86-64")
	if got := len(libs); got != 1 {
		t.Fatalf("parseNVIDIALibraries() returned %d libs, want 1 (deduped)", got)
	}
	// First match wins.
	if libs[0].HostPath != "/usr/lib/x86_64-linux-gnu/libcuda.so.1" {
		t.Errorf("expected first match to win, got %q", libs[0].HostPath)
	}
}

func TestParseNVIDIALibraries_IncludesVDPAU(t *testing.T) {
	libs := parseNVIDIALibraries([]byte(
		"libvdpau_nvidia.so.580.126.09 (libc6,x86-64) => /usr/lib/x86_64-linux-gnu/vdpau/libvdpau_nvidia.so.580.126.09\n",
	), "x86-64")

	if got := len(libs); got != 1 {
		t.Fatalf("parseNVIDIALibraries() returned %d libs, want 1", got)
	}

	if got, want := libs[0].HostPath, "/usr/lib/x86_64-linux-gnu/vdpau/libvdpau_nvidia.so.580.126.09"; got != want {
		t.Errorf("HostPath = %q, want %q", got, want)
	}
}

func TestResolveNVIDIAI386LibrariesScansMatchingDriverDirectory(t *testing.T) {
	t.Parallel()

	libDir := t.TempDir()
	version := "580.126.09"
	files := []string{
		"libGLX_nvidia.so." + version,
		"libGLX_nvidia.so.0",
		"libcudadebugger.so." + version,
		"libGLdispatch.so.0",
		"libOpenCL.so.1",
	}

	for _, name := range files {
		require.NoError(t, os.WriteFile(filepath.Join(libDir, name), []byte(name), 0o644))
	}

	vdpauDir := filepath.Join(libDir, "vdpau")
	vdpauName := "libvdpau_nvidia.so." + version

	require.NoError(t, os.Mkdir(vdpauDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(vdpauDir, vdpauName), []byte(vdpauName), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(libDir, "libpthread.so.0"), nil, 0o644))

	libs, mounts := resolveNVIDIAI386Libraries(version, []string{libDir})
	libNames := make([]string, 0, len(libs))

	for _, lib := range libs {
		libNames = append(libNames, filepath.Base(lib.HostPath))

		mountIndex := "0"
		if filepath.Base(filepath.Dir(lib.HostPath)) == "vdpau" {
			mountIndex = "1"
		}

		require.Equal(t, filepath.Join(NvidiaHostI386LibDir, mountIndex, filepath.Base(lib.HostPath)), lib.ContainerPath)
	}

	require.ElementsMatch(t, append(files, vdpauName), libNames)
	require.Len(t, mounts, 2)
	require.Equal(t, libDir, mounts[0].HostDir)
	require.Equal(t, "/run/host-nvidia-i386/0", mounts[0].ContainerDir)
	require.Equal(t, vdpauDir, mounts[1].HostDir)
	require.Equal(t, "/run/host-nvidia-i386/1", mounts[1].ContainerDir)
}

func TestResolveNVIDIAI386LibrariesRejectsMismatchedDriver(t *testing.T) {
	t.Parallel()

	libDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(libDir, "libGLX_nvidia.so.570.86.15"), nil, 0o644))

	libs, mounts := resolveNVIDIAI386Libraries("580.126.09", []string{libDir})
	require.Empty(t, libs)
	require.Empty(t, mounts)
}

func Test_buildNVIDIALibMounts(t *testing.T) {
	libs := []NvidiaLibMapping{
		{HostPath: "/usr/lib/x86_64-linux-gnu/libcuda.so.1"},
		{HostPath: "/usr/lib/x86_64-linux-gnu/libnvidia-ml.so.1"},
		{HostPath: "/usr/lib64/libnvoptix.so.1"},
	}

	libs, mounts := buildNVIDIALibMounts(libs, "/usr/lib/x86_64-linux-gnu")

	// Two unique directories.
	if got := len(mounts); got != 2 {
		t.Fatalf("buildNVIDIALibMounts() returned %d mounts, want 2", got)
	}

	// Verify sorted by container path and index matches.
	if mounts[0].ContainerDir != "/run/host-nvidia/0" {
		t.Errorf("mounts[0].ContainerDir = %q, want /run/host-nvidia/0", mounts[0].ContainerDir)
	}

	if mounts[0].Index != 0 {
		t.Errorf("mounts[0].Index = %d, want 0", mounts[0].Index)
	}

	if mounts[1].ContainerDir != "/run/host-nvidia/1" {
		t.Errorf("mounts[1].ContainerDir = %q, want /run/host-nvidia/1", mounts[1].ContainerDir)
	}

	if mounts[1].Index != 1 {
		t.Errorf("mounts[1].Index = %d, want 1", mounts[1].Index)
	}

	// Verify container paths are stamped on library mappings.
	// /usr/lib/x86_64-linux-gnu sorts before /usr/lib64 ('/' < '6'), so:
	//   index 0 = /usr/lib/x86_64-linux-gnu -> /run/host-nvidia/0
	//   index 1 = /usr/lib64 -> /run/host-nvidia/1
	if libs[0].ContainerPath != "/run/host-nvidia/0/libcuda.so.1" {
		t.Errorf("libs[0].ContainerPath = %q, want /run/host-nvidia/0/libcuda.so.1", libs[0].ContainerPath)
	}

	if libs[2].ContainerPath != "/run/host-nvidia/1/libnvoptix.so.1" {
		t.Errorf("libs[2].ContainerPath = %q, want /run/host-nvidia/1/libnvoptix.so.1", libs[2].ContainerPath)
	}
}

func Test_buildNVIDIALibMounts_SingleDirectory(t *testing.T) {
	libs := []NvidiaLibMapping{
		{HostPath: "/usr/lib64/libcuda.so.1"},
		{HostPath: "/usr/lib64/libnvidia-ml.so"},
	}

	libs, mounts := buildNVIDIALibMounts(libs, "/usr/lib/x86_64-linux-gnu")
	if got := len(mounts); got != 1 {
		t.Fatalf("buildNVIDIALibMounts() returned %d mounts, want 1", got)
	}

	// All libs should point into the same mount.
	for i, lib := range libs {
		if lib.ContainerPath == "" {
			t.Errorf("libs[%d].ContainerPath is empty", i)
		}
	}

	if libs[0].ContainerPath != "/run/host-nvidia/0/libcuda.so.1" {
		t.Errorf("libs[0].ContainerPath = %q, want /run/host-nvidia/0/libcuda.so.1", libs[0].ContainerPath)
	}
}

func Test_buildNVIDIALibMounts_Empty(t *testing.T) {
	libs, mounts := buildNVIDIALibMounts(nil, "/usr/lib/x86_64-linux-gnu")
	if libs != nil {
		t.Errorf("buildNVIDIALibMounts(nil) returned non-nil libs")
	}

	if mounts != nil {
		t.Errorf("buildNVIDIALibMounts(nil) returned non-nil mounts")
	}
}
