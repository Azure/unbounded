// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package goalstates

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// NVIDIA host discovery and goal state resolution.
//
// When the NVIDIA driver is installed on the host but the nspawn container
// uses a separate rootfs (e.g. an OCI image), the driver's userspace
// libraries (libnvidia-ml, libcuda, etc.) are only present on the host.
// Tools that need them inside the container, in particular nvidia-ctk for
// CDI spec generation, will fail with "ERROR_LIBRARY_NOT_FOUND".
//
// The library discovery and bind-mount approach is derived from the intuneme
// project (https://github.com/frostyard/intuneme).
//
//  1. Parse `ldconfig -p` on the host to find NVIDIA library directories for
//     the host architecture (x86-64 or aarch64), then scan those directories
//     for aliases and real versioned files.
//  2. Bind-mount the host directories containing those libraries into the
//     nspawn container at /run/host-nvidia/0/, /run/host-nvidia/1/, etc.
//  3. After the nspawn boots, create symlinks in the container's standard
//     multiarch library path pointing into the bind mounts and run ldconfig
//     to update the linker cache.

// NvidiaHost aggregates all NVIDIA-related host state discovered at agent
// startup: GPU device paths, driver library mappings, and the derived
// bind-mount specifications for the nspawn container.
type NvidiaHost struct {
	// GPUDevicePaths lists NVIDIA GPU device paths discovered on the host
	// (e.g. /dev/nvidia0, /dev/nvidiactl, /dev/nvidia-caps/*,
	// /dev/nvidia-caps-imex-channels/*, /dev/dri/*).
	// When non-empty the nspawn configuration will bind-mount these devices
	// and grant the container cgroup access to them.
	GPUDevicePaths []string

	// ContainerLibDir is the architecture-specific multiarch library
	// directory inside the nspawn container (e.g. /usr/lib/x86_64-linux-gnu
	// on amd64, /usr/lib/aarch64-linux-gnu on arm64). Symlinks to
	// bind-mounted host NVIDIA libraries are created here.
	ContainerLibDir string

	// LibMappings contains NVIDIA userspace libraries discovered from
	// ldconfig -p and filesystem scans. These are used to create symlinks
	// inside the nspawn container so that the host's NVIDIA driver libraries
	// are accessible.
	LibMappings []NvidiaLibMapping

	// LibDirMounts lists unique host directories containing NVIDIA libraries
	// to be bind-mounted read-only into the nspawn container at
	// /run/host-nvidia/<index>/. After boot, symlinks from the container's
	// standard library path are created by the setup-nvidia-libraries task.
	LibDirMounts []NvidiaLibDirMount

	// I386LibMappings contains optional i386 NVIDIA and GLVND libraries with a
	// driver version matching the active 64-bit driver.
	I386LibMappings []NvidiaLibMapping

	// I386LibDirMounts lists the host directories for I386LibMappings.
	I386LibDirMounts []NvidiaLibDirMount

	// NvidiaSMIPath is the host path to nvidia-smi, when available.
	NvidiaSMIPath string

	// DriverVersion is the active NVIDIA kernel driver version. It is used to
	// provide versioned library names when a host installation exposes only
	// unversioned or SONAME aliases through ldconfig.
	DriverVersion string
}

// NvidiaLibMapping maps a host NVIDIA library to its corresponding paths
// inside the nspawn container.
type NvidiaLibMapping struct {
	HostPath         string // e.g. "/usr/lib/x86_64-linux-gnu/libcuda.so.1", original discovered name
	ResolvedHostPath string // e.g. "/usr/lib/nvidia/libcuda.so.580.126.09", real bind-mount source
	ContainerPath    string // e.g. "/run/host-nvidia/0/libcuda.so.580.126.09", bind-mount source
	LinkPath         string // e.g. "/usr/lib/x86_64-linux-gnu/libcuda.so.1", symlink in container
}

// NvidiaLibDirMount represents a read-only bind mount of a host directory
// containing NVIDIA libraries into the nspawn container.
type NvidiaLibDirMount struct {
	Index        int    // mount index, used by symlink creation to map libs to their container path
	HostDir      string // e.g. "/usr/lib/x86_64-linux-gnu"
	ContainerDir string // e.g. "/run/host-nvidia/0"
}

// NvidiaRuntime describes the NVIDIA container runtime configuration for
// containerd. When Enabled is true the runtime is registered as a handler
// so that GPU workloads can be scheduled.
type NvidiaRuntime struct {
	Enabled                    bool
	RuntimeClassName           string
	RuntimePath                string
	DisableSetAsDefaultRuntime bool
}

// ResolveNvidiaHost probes the host for NVIDIA GPU devices and driver
// libraries, returning a fully populated NvidiaHost. The arch parameter is a
// GOARCH value (e.g. "amd64", "arm64") used to select the correct multiarch
// library path and ldconfig filter. Returns an error for unsupported
// architectures. On a non-GPU host the returned struct has all nil/empty
// fields (except ContainerLibDir).
func ResolveNvidiaHost(arch string) (NvidiaHost, error) {
	archInfo, ok := nvidiaArchMap[arch]
	if !ok {
		return NvidiaHost{}, fmt.Errorf("unsupported architecture %q for NVIDIA library discovery", arch)
	}

	devices := discoverNVIDIADevices()
	libs, mounts, i386Libs, i386Mounts, driverVersion := resolveNVIDIALibraries(archInfo)

	return NvidiaHost{
		GPUDevicePaths:   devices,
		ContainerLibDir:  archInfo.libDir,
		LibMappings:      libs,
		LibDirMounts:     mounts,
		I386LibMappings:  i386Libs,
		I386LibDirMounts: i386Mounts,
		NvidiaSMIPath:    discoverNVIDIASMI(),
		DriverVersion:    driverVersion,
	}, nil
}

// resolveNvidiaRuntime returns the NVIDIA container runtime goal state.
// When GPU devices are present the runtime is enabled with default paths;
// otherwise it is disabled.
func resolveNvidiaRuntime() NvidiaRuntime {
	return NvidiaRuntime{
		Enabled:                    len(discoverNVIDIADevices()) > 0,
		RuntimeClassName:           NvidiaRuntimeClassName,
		RuntimePath:                NvidiaContainerRuntimePath,
		DisableSetAsDefaultRuntime: false,
	}
}

const (
	devDir            = "/dev"
	nvidiaCapsDirName = "nvidia-caps"
	nvidiaIMEXDirName = "nvidia-caps-imex-channels"
	driDir            = "/dev/dri"
	nvidiaDevPrefix   = "nvidia"
)

// nvidiaArch contains architecture-specific values for NVIDIA library
// discovery and symlink creation inside the nspawn container.
type nvidiaArch struct {
	// ldconfigTag is the architecture identifier in ldconfig -p output
	// (e.g. "x86-64", "aarch64"). Used to filter libraries to the
	// correct architecture and avoid multilib collisions.
	ldconfigTag string

	// libDir is the Debian/Ubuntu multiarch library directory for this
	// architecture (e.g. "/usr/lib/x86_64-linux-gnu"). Symlinks to
	// bind-mounted host NVIDIA libraries are created here inside the
	// nspawn container.
	libDir string

	// i386LibDir is the optional 32-bit library directory on amd64 hosts.
	i386LibDir string
}

// nvidiaArchMap maps GOARCH values to their NVIDIA-specific arch parameters.
var nvidiaArchMap = map[string]nvidiaArch{
	"amd64": {ldconfigTag: "x86-64", libDir: "/usr/lib/x86_64-linux-gnu", i386LibDir: "/usr/lib/i386-linux-gnu"},
	"arm64": {ldconfigTag: "aarch64", libDir: "/usr/lib/aarch64-linux-gnu"},
}

// discoverNVIDIADevices scans /dev for NVIDIA device nodes and returns them
// as a sorted slice of device paths. The following device nodes are collected
// when present:
//
//   - /dev/nvidia0, /dev/nvidia1, ...  (per-GPU devices)
//   - /dev/nvidiactl                   (control device)
//   - /dev/nvidia-modeset              (modeset interface)
//   - /dev/nvidia-uvm                  (unified virtual memory)
//   - /dev/nvidia-uvm-tools            (UVM tools interface)
//   - /dev/nvidia-caps/*               (capability devices)
//   - /dev/nvidia-caps-imex-channels/* (IMEX channel devices)
//   - /dev/dri/card*, /dev/dri/renderD* (DRI render nodes, needed by CDI and
//     some GPU workloads such as OpenGL/Vulkan)
//
// Returns nil (not an error) when no NVIDIA devices are found; the host
// simply does not have NVIDIA GPUs or the driver is not loaded.
func discoverNVIDIADevices() []string {
	return discoverNVIDIADevicesIn(devDir)
}

func discoverNVIDIADevicesIn(deviceDir string) []string {
	var devices []string

	entries, err := os.ReadDir(deviceDir)
	if err != nil {
		return nil
	}

	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, nvidiaDevPrefix) {
			continue
		}

		// Skip directories at the top level (nvidia-caps is handled below).
		if e.IsDir() {
			continue
		}

		devices = append(devices, filepath.Join(deviceDir, name))
	}

	// Collect /dev/nvidia-caps/* entries (e.g. nvidia-cap1, nvidia-cap2).
	capsDir := filepath.Join(deviceDir, nvidiaCapsDirName)

	capsEntries, err := os.ReadDir(capsDir)
	if err == nil {
		for _, e := range capsEntries {
			if e.IsDir() {
				continue
			}

			devices = append(devices, filepath.Join(capsDir, e.Name()))
		}
	}

	// Collect IMEX channel devices used for multi-node NVLink.
	imexDir := filepath.Join(deviceDir, nvidiaIMEXDirName)

	imexEntries, err := os.ReadDir(imexDir)
	if err == nil {
		for _, e := range imexEntries {
			if e.IsDir() {
				continue
			}

			devices = append(devices, filepath.Join(imexDir, e.Name()))
		}
	}

	// When NVIDIA devices are present, also collect /dev/dri/* entries.
	// These DRI render nodes are created by the NVIDIA driver and are
	// referenced by the CDI specification generated by nvidia-ctk.
	// Without them, CDI-based container creation fails with ENOENT.
	if len(devices) > 0 {
		discoveredDRIDir := filepath.Join(deviceDir, filepath.Base(driDir))

		driEntries, err := os.ReadDir(discoveredDRIDir)
		if err == nil {
			for _, e := range driEntries {
				if e.IsDir() {
					continue
				}

				devices = append(devices, filepath.Join(discoveredDRIDir, e.Name()))
			}
		}
	}

	slices.Sort(devices)

	return devices
}

// nvidiaLibPrefixes are the library name prefixes collected from ldconfig output.
var nvidiaLibPrefixes = []string{
	"libnvidia-",
	"libcuda",
	"libcudadebugger",
	"libEGL_nvidia",
	"libGLX_nvidia",
	"libGLESv1_CM_nvidia",
	"libGLESv2_nvidia",
	"libnvcuvid",
	"libnvoptix",
	"libvdpau_nvidia",
}

// nvidiaLibGlobs includes both linker aliases and the real, versioned driver
// files. ldconfig -p commonly reports only the aliases, while nvidia-ctk
// explicitly looks for names such as libcuda.so.<driver-version>.
var nvidiaLibGlobs = []string{
	"libcuda.so*",
	"libcudadebugger.so*",
	"libnvidia*.so*",
	"libEGL_nvidia.so*",
	"libGLX_nvidia.so*",
	"libGLESv*_nvidia.so*",
	"libnvcuvid.so*",
	"libnvoptix.so*",
	"libvdpau_nvidia.so*",
}

// nvidiaI386LibGlobs also includes the architecture-neutral GLVND and OpenCL
// loader names needed by 32-bit applications.
var nvidiaI386LibGlobs = []string{
	"libcuda.so*",
	"libcudadebugger.so*",
	"libnvcuvid.so*",
	"libnvoptix.so*",
	"libnvidia*.so*",
	"libEGL_nvidia.so*",
	"libGLX_nvidia.so*",
	"libGLESv1_CM_nvidia.so*",
	"libGLESv2_nvidia.so*",
	"libEGL.so*",
	"libGL.so*",
	"libGLESv1_CM.so*",
	"libGLESv2.so*",
	"libGLX.so*",
	"libGLX_indirect.so*",
	"libOpenGL.so*",
	"libGLdispatch.so*",
	"libOpenCL.so*",
	"libvdpau_nvidia.so*",
}

// resolveNVIDIALibraries runs ldconfig -p on the host and returns enriched
// library mappings and their corresponding bind-mount specs.
func resolveNVIDIALibraries(arch nvidiaArch) ([]NvidiaLibMapping, []NvidiaLibDirMount, []NvidiaLibMapping, []NvidiaLibDirMount, string) {
	out, err := exec.Command("ldconfig", "-p").Output()
	if err != nil {
		return nil, nil, nil, nil, ""
	}

	libs := parseNVIDIALibraries(out, arch.ldconfigTag)
	libs = expandNVIDIALibraries(libs)

	driverVersion := discoverNVIDIADriverVersion(libs, "/sys/module/nvidia/version")
	libs, mounts := buildNVIDIALibMounts(libs, arch.libDir)

	i386Libs, i386Mounts := resolveNVIDIAI386Libraries(driverVersion, nvidiaI386LibraryDirs(arch.i386LibDir))

	return libs, mounts, i386Libs, i386Mounts, driverVersion
}

func discoverNVIDIASMI() string {
	for _, path := range []string{"/usr/bin/nvidia-smi", "/usr/local/bin/nvidia-smi"} {
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return path
		}
	}

	return ""
}

// expandNVIDIALibraries scans every library directory selected by ldconfig for
// NVIDIA aliases and versioned files. It also follows alternatives-style
// symlinks so their real targets are included in the bind mounts.
func expandNVIDIALibraries(libs []NvidiaLibMapping) []NvidiaLibMapping {
	if len(libs) == 0 {
		return nil
	}

	expanded := slices.Clone(libs)
	seenPaths := make(map[string]bool, len(libs))
	seenNames := make(map[string]bool, len(libs))
	dirs := make(map[string]bool)

	for i := range expanded {
		expanded[i].ResolvedHostPath = resolveNVIDIALibraryPath(expanded[i].HostPath)
		seenPaths[expanded[i].HostPath] = true
		seenNames[filepath.Base(expanded[i].HostPath)] = true
		dirs[filepath.Dir(expanded[i].HostPath)] = true
	}

	var matches []string

	for dir := range dirs {
		for _, pattern := range nvidiaLibGlobs {
			for _, searchDir := range []string{dir, filepath.Join(dir, "vdpau")} {
				found, err := filepath.Glob(filepath.Join(searchDir, pattern))
				if err == nil {
					matches = append(matches, found...)
				}
			}
		}
	}

	sort.Strings(matches)

	add := func(path string) {
		name := filepath.Base(path)
		if seenPaths[path] || seenNames[name] || !isNVIDIALibraryName(name) {
			return
		}

		info, err := os.Lstat(path)
		if err != nil || info.IsDir() {
			return
		}

		seenPaths[path] = true
		seenNames[name] = true

		expanded = append(expanded, NvidiaLibMapping{
			HostPath:         path,
			ResolvedHostPath: resolveNVIDIALibraryPath(path),
		})
	}

	for _, path := range matches {
		add(path)
	}

	// Resolve all aliases, including the original ldconfig entries. A target
	// may live outside the alias directory, for example under alternatives.
	for _, lib := range slices.Clone(expanded) {
		target, err := filepath.EvalSymlinks(lib.HostPath)
		if err == nil {
			add(target)
		}
	}

	return expanded
}

func resolveNVIDIALibraryPath(path string) string {
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return resolved
	}

	return path
}

func isNVIDIALibraryName(name string) bool {
	for _, pattern := range nvidiaLibGlobs {
		matched, err := filepath.Match(pattern, name)
		if err == nil && matched {
			return true
		}
	}

	return false
}

func discoverNVIDIADriverVersion(libs []NvidiaLibMapping, moduleVersionPath string) string {
	// The active SONAME symlinks are the strongest userspace signal because a
	// directory can contain files left behind by an older driver package.
	for _, preferredName := range []string{"libcuda.so.1", "libnvidia-ml.so.1"} {
		for _, lib := range libs {
			if filepath.Base(lib.HostPath) != preferredName {
				continue
			}

			target, err := filepath.EvalSymlinks(lib.HostPath)
			if err == nil {
				if version := nvidiaDriverVersionFromName(filepath.Base(target)); version != "" {
					return version
				}
			}
		}
	}

	if data, err := os.ReadFile(moduleVersionPath); err == nil {
		if version := strings.TrimSpace(string(data)); isNVIDIADriverVersion(version) {
			return version
		}
	}

	for _, preferredPrefix := range []string{"libGLX_nvidia.so.", "libcuda.so.", "libnvidia-ml.so."} {
		for _, lib := range libs {
			name := filepath.Base(lib.HostPath)
			if !strings.HasPrefix(name, preferredPrefix) {
				continue
			}

			if version := nvidiaDriverVersionFromName(name); version != "" {
				return version
			}
		}
	}

	return ""
}

func nvidiaDriverVersionFromName(name string) string {
	_, version, found := strings.Cut(name, ".so.")
	if found && isNVIDIADriverVersion(version) {
		return version
	}

	return ""
}

func isNVIDIADriverVersion(version string) bool {
	return strings.Count(version, ".") >= 2 && strings.Trim(version, "0123456789.") == ""
}

func nvidiaI386LibraryDirs(i386LibDir string) []string {
	if i386LibDir == "" {
		return nil
	}

	dirs := []string{i386LibDir}
	if strings.HasPrefix(i386LibDir, "/usr/") {
		dirs = append(dirs, strings.TrimPrefix(i386LibDir, "/usr"))
	}

	return dirs
}

// resolveNVIDIAI386Libraries finds a compat32 installation matching the active
// driver version. This intentionally scans the filesystem because ldconfig -p
// often lists only its SONAME aliases.
func resolveNVIDIAI386Libraries(driverVersion string, candidateDirs []string) ([]NvidiaLibMapping, []NvidiaLibDirMount) {
	if driverVersion == "" {
		return nil, nil
	}

	seenDirs := make(map[string]bool)
	seenNames := make(map[string]bool)

	var libs []NvidiaLibMapping

	for _, candidateDir := range candidateDirs {
		dir, err := filepath.EvalSymlinks(candidateDir)
		if err != nil || seenDirs[dir] || !hasNVIDIADriverVersionInDir(dir, driverVersion) {
			continue
		}

		seenDirs[dir] = true

		var matches []string

		for _, pattern := range nvidiaI386LibGlobs {
			for _, searchDir := range []string{dir, filepath.Join(dir, "vdpau")} {
				found, globErr := filepath.Glob(filepath.Join(searchDir, pattern))
				if globErr == nil {
					matches = append(matches, found...)
				}
			}
		}

		sort.Strings(matches)

		for _, path := range matches {
			name := filepath.Base(path)
			if seenNames[name] {
				continue
			}

			info, statErr := os.Lstat(path)
			if statErr != nil || info.IsDir() {
				continue
			}

			seenNames[name] = true

			libs = append(libs, NvidiaLibMapping{
				HostPath:         path,
				ResolvedHostPath: resolveNVIDIALibraryPath(path),
			})
		}
	}

	return buildNVIDIALibMountsAt(libs, "/usr/lib/i386-linux-gnu", NvidiaHostI386LibDir)
}

func hasNVIDIADriverVersionInDir(dir, driverVersion string) bool {
	for _, family := range []string{"libGLX_nvidia.so.", "libnvidia-ml.so.", "libcuda.so."} {
		path := filepath.Join(dir, family+driverVersion)
		if info, err := os.Lstat(path); err == nil && !info.IsDir() {
			return true
		}
	}

	return false
}

// buildNVIDIALibMounts takes parsed library mappings (from parseNVIDIALibraries),
// deduplicates their parent directories into bind-mount specs, and stamps each
// mapping with its container-side path. containerLibDir is the multiarch
// library directory inside the container where symlinks will be created.
func buildNVIDIALibMounts(libs []NvidiaLibMapping, containerLibDir string) ([]NvidiaLibMapping, []NvidiaLibDirMount) {
	return buildNVIDIALibMountsAt(libs, containerLibDir, NvidiaHostLibDir)
}

func buildNVIDIALibMountsAt(libs []NvidiaLibMapping, containerLibDir, mountBaseDir string) ([]NvidiaLibMapping, []NvidiaLibDirMount) {
	if len(libs) == 0 {
		return nil, nil
	}

	// Collect unique host directories and sort for deterministic index
	// assignment regardless of ldconfig output ordering.
	seen := make(map[string]bool)

	var dirs []string

	for _, lib := range libs {
		dir := filepath.Dir(nvidiaLibSourceHostPath(lib))
		if seen[dir] {
			continue
		}

		seen[dir] = true
		dirs = append(dirs, dir)
	}

	sort.Strings(dirs)

	// Build mounts and a dir → container-dir lookup in one pass.
	dirToContainer := make(map[string]string, len(dirs))
	mounts := make([]NvidiaLibDirMount, len(dirs))

	for i, dir := range dirs {
		containerDir := fmt.Sprintf("%s/%d", mountBaseDir, i)
		mounts[i] = NvidiaLibDirMount{
			Index:        i,
			HostDir:      dir,
			ContainerDir: containerDir,
		}

		dirToContainer[dir] = containerDir
	}

	// Stamp each library mapping with its resolved container source and its
	// original destination name. This avoids depending on absolute host
	// symlinks resolving inside the nspawn machine.
	for i := range libs {
		sourcePath := nvidiaLibSourceHostPath(libs[i])
		libs[i].ContainerPath = filepath.Join(
			dirToContainer[filepath.Dir(sourcePath)],
			filepath.Base(sourcePath),
		)
		libs[i].LinkPath = filepath.Join(containerLibDir, filepath.Base(libs[i].HostPath))
	}

	return libs, mounts
}

func nvidiaLibSourceHostPath(lib NvidiaLibMapping) string {
	if lib.ResolvedHostPath != "" {
		return lib.ResolvedHostPath
	}

	return lib.HostPath
}

// parseNVIDIALibraries extracts NVIDIA library mappings from ldconfig -p
// output. Only libraries matching the given architecture tag (e.g. "x86-64",
// "aarch64") are included to avoid multilib collisions.
func parseNVIDIALibraries(ldconfigOutput []byte, archTag string) []NvidiaLibMapping {
	var libs []NvidiaLibMapping

	seen := make(map[string]bool)

	scanner := bufio.NewScanner(bytes.NewReader(ldconfigOutput))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		// ldconfig -p lines look like:
		//   libcuda.so.1 (libc6,x86-64) => /usr/lib/x86_64-linux-gnu/libcuda.so.1
		if !strings.Contains(line, "=>") {
			continue
		}

		// Filter to the target architecture only when an architecture was given.
		if archTag != "" && !strings.Contains(line, archTag) {
			continue
		}

		isNvidia := false

		for _, prefix := range nvidiaLibPrefixes {
			if strings.HasPrefix(line, prefix) {
				isNvidia = true
				break
			}
		}

		if !isNvidia {
			continue
		}

		// Extract the path after "=> ".
		parts := strings.SplitN(line, "=> ", 2)
		if len(parts) != 2 {
			continue
		}

		hostPath := strings.TrimSpace(parts[1])
		if hostPath == "" {
			continue
		}

		basename := filepath.Base(hostPath)

		// Deduplicate by basename for an architecture-specific lookup, where
		// ldconfig priority determines the preferred library. Keep paths
		// distinct when collecting optional i386 directories.
		key := basename
		if archTag == "" {
			key = hostPath
		}

		if seen[key] {
			continue
		}

		seen[key] = true

		libs = append(libs, NvidiaLibMapping{HostPath: hostPath})
	}

	return libs
}
