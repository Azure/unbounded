// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package nodestart

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Azure/unbounded/internal/executil"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

type setupNVIDIA struct {
	log       *slog.Logger
	goalState *goalstates.NodeStart
}

// SetupNVIDIA returns a task that makes NVIDIA driver libraries from the host
// accessible inside the running nspawn machine and generates a CDI specification
// describing the available GPUs.
//
// The task performs four steps in sequence:
//  1. Creates symlinks in the container's standard library path pointing into
//     the bind-mounted /run/host-nvidia/ directories, then runs ldconfig.
//  2. Builds a self-contained driver root under /run/nvidia/driver and creates
//     its dynamic linker cache.
//  3. Ensures the CDI spec directory exists.
//  4. Runs nvidia-ctk cdi generate against that driver root to produce the CDI
//     spec at /etc/cdi/nvidia.yaml. Most hooks are disabled because they interfere
//     with the nspawn environment (e.g. mounting tmpfs over /proc paths, CUDA
//     compat hooks on NVSwitch systems). Only create-symlinks and update-ldcache
//     are retained.
//
// This task must run after StartNSpawnMachine (the machine must be booted and
// the /run/host-nvidia/ bind mounts must be active) and before StartKubelet
// (so pods can use GPUs immediately).
//
// The task is a no-op when the NVIDIA runtime is not enabled or no library
// mappings were discovered on the host.
func SetupNVIDIA(log *slog.Logger, goalState *goalstates.NodeStart) phases.Task {
	return &setupNVIDIA{log: log, goalState: goalState}
}

func (s *setupNVIDIA) Name() string { return "setup-nvidia" }

func (s *setupNVIDIA) Do(ctx context.Context) error {
	if !s.goalState.Nvidia.Required {
		s.log.Info("NVIDIA was not provisioned for this machine, skipping")

		return nil
	}

	if !s.goalState.Containerd.NvidiaRuntime.Enabled || !goalstates.NVIDIAStateAvailable(s.goalState.Nvidia) {
		return fmt.Errorf("NVIDIA is required but resolved setup state is incomplete")
	}

	if err := s.setupLibraries(ctx); err != nil {
		return err
	}

	if err := s.prepareDriverRoot(ctx); err != nil {
		return err
	}

	if err := s.generateCDISpec(ctx); err != nil {
		return err
	}

	return nil
}

func (s *setupNVIDIA) setupLibraries(ctx context.Context) error {
	libs := s.goalState.Nvidia.LibMappings
	machine := s.goalState.MachineName

	s.log.Info("setting up NVIDIA library symlinks inside nspawn machine",
		slog.String("machine", machine),
		slog.Int("libraries", len(libs)),
	)

	// Clean stale symlinks from a previous session that may point into
	// /run/host-nvidia/ paths that no longer exist.
	if _, err := executil.MachineRun(ctx, s.log, machine,
		"find", s.goalState.Nvidia.ContainerLibDir, "-maxdepth", "1",
		"-lname", goalstates.NvidiaHostLibDir+"/*", "-delete",
	); err != nil {
		return fmt.Errorf("removing stale nvidia symlinks: %w", err)
	}

	// Create symlinks: <lib>.LinkPath -> <lib>.ContainerPath
	for _, lib := range libs {
		// Remove any existing file/symlink, then create the new symlink.
		// Errors from rm -f are intentionally ignored - the file may not exist.
		executil.MachineRun(ctx, s.log, machine, "rm", "-f", lib.LinkPath) //nolint:errcheck // rm -f is best-effort.

		if _, err := executil.MachineRun(ctx, s.log, machine,
			"ln", "-s", lib.ContainerPath, lib.LinkPath,
		); err != nil {
			return fmt.Errorf("symlink %s -> %s: %w", lib.LinkPath, lib.ContainerPath, err)
		}

		s.log.Debug("linked NVIDIA library",
			slog.String("source", lib.ContainerPath),
			slog.String("destination", lib.LinkPath),
		)
	}

	// Update the dynamic linker cache so the libraries are discoverable.
	if _, err := executil.MachineRun(ctx, s.log, machine, "ldconfig"); err != nil {
		return fmt.Errorf("ldconfig failed: %w", err)
	}

	s.log.Info("NVIDIA library symlinks created and ldconfig updated",
		slog.Int("count", len(libs)),
	)

	return nil
}

type nvidiaDriverRootPaths struct {
	rootDir    string
	libDir     string
	i386LibDir string
	libDirName string
}

func (s *setupNVIDIA) prepareDriverRoot(ctx context.Context) error {
	if s.goalState.Nvidia.DriverVersion == "" {
		return fmt.Errorf("cannot prepare NVIDIA driver root: active driver version was not detected")
	}

	paths := s.driverRootPaths()

	if err := s.initializeDriverRoot(ctx, paths); err != nil {
		return err
	}

	if err := s.copyDriverLibraries(ctx, paths); err != nil {
		return err
	}

	if err := s.copyCompat32DriverLibraries(ctx, paths); err != nil {
		return err
	}

	if err := s.copyNVIDIASMI(ctx, paths); err != nil {
		return err
	}

	if err := s.copyNVIDIAIMEXBinaries(ctx, paths); err != nil {
		return err
	}

	if err := s.createDriverRootLinks(ctx, paths); err != nil {
		return err
	}

	if err := s.generateDriverRootLinkerCache(ctx, paths); err != nil {
		return err
	}

	return s.validateDriverRoot(ctx, paths)
}

func (s *setupNVIDIA) driverRootPaths() nvidiaDriverRootPaths {
	libDirName := filepath.Base(s.goalState.Nvidia.ContainerLibDir)

	return nvidiaDriverRootPaths{
		rootDir:    goalstates.NvidiaDriverDir,
		libDir:     filepath.Join(goalstates.NvidiaDriverDir, "lib", libDirName),
		i386LibDir: filepath.Join(goalstates.NvidiaDriverDir, "lib", "i386-linux-gnu"),
		libDirName: libDirName,
	}
}

func (s *setupNVIDIA) initializeDriverRoot(ctx context.Context, paths nvidiaDriverRootPaths) error {
	machine := s.goalState.MachineName
	if _, err := executil.MachineRun(ctx, s.log, machine, "rm", "-rf", paths.rootDir); err != nil {
		return fmt.Errorf("remove NVIDIA driver root: %w", err)
	}

	// On amd64, the resolved command is equivalent to:
	// mkdir -p /run/nvidia/driver/lib/x86_64-linux-gnu \
	//   /run/nvidia/driver/lib/i386-linux-gnu /run/nvidia/driver/usr/bin \
	//   /run/nvidia/driver/usr/lib /run/nvidia/driver/sbin /run/nvidia/driver/etc
	if _, err := executil.MachineRun(ctx, s.log, machine,
		"mkdir", "-p", paths.libDir, paths.i386LibDir,
		filepath.Join(paths.rootDir, "usr", "bin"),
		filepath.Join(paths.rootDir, "usr", "lib"),
		filepath.Join(paths.rootDir, "sbin"),
		filepath.Join(paths.rootDir, "etc"),
	); err != nil {
		return fmt.Errorf("create NVIDIA driver root: %w", err)
	}

	return nil
}

func (s *setupNVIDIA) copyDriverLibraries(ctx context.Context, paths nvidiaDriverRootPaths) error {
	for _, lib := range s.goalState.Nvidia.LibMappings {
		if err := s.copyDriverLibrary(ctx, lib, paths.libDir); err != nil {
			return err
		}
	}

	// Some installations contain only .so and .so.1 files even after the
	// filesystem scan. Materialize real files under the active driver version
	// because nvidia-ctk requires those names for core libraries.
	for _, copy := range versionedNVIDIALibraryCopies(s.goalState.Nvidia.LibMappings, s.goalState.Nvidia.DriverVersion) {
		destination := filepath.Join(paths.libDir, copy.relativeDestination)
		if _, err := executil.MachineRun(ctx, s.log, s.goalState.MachineName, "mkdir", "-p", filepath.Dir(destination)); err != nil {
			return fmt.Errorf("create directory for versioned NVIDIA library: %w", err)
		}

		if _, err := executil.MachineRun(ctx, s.log, s.goalState.MachineName, "cp", "-aL", copy.source, destination); err != nil {
			return fmt.Errorf("create versioned NVIDIA library %s: %w", destination, err)
		}

		s.log.Debug("copied versioned NVIDIA driver library",
			slog.String("source", copy.source),
			slog.String("destination", destination),
		)
	}

	return nil
}

func (s *setupNVIDIA) copyCompat32DriverLibraries(ctx context.Context, paths nvidiaDriverRootPaths) error {
	if paths.libDirName == "x86_64-linux-gnu" && len(s.goalState.Nvidia.I386LibMappings) == 0 {
		s.log.Warn("matching NVIDIA compat32 libraries were not found",
			slog.String("driverVersion", s.goalState.Nvidia.DriverVersion),
		)
	}

	for _, lib := range s.goalState.Nvidia.I386LibMappings {
		if err := s.copyDriverLibrary(ctx, lib, paths.i386LibDir); err != nil {
			return err
		}
	}

	return nil
}

func (s *setupNVIDIA) copyDriverLibrary(ctx context.Context, lib goalstates.NvidiaLibMapping, destinationDir string) error {
	destination := filepath.Join(destinationDir, filepath.Base(lib.HostPath))
	if filepath.Base(filepath.Dir(lib.HostPath)) == "vdpau" {
		destination = filepath.Join(destinationDir, "vdpau", filepath.Base(lib.HostPath))
		if _, err := executil.MachineRun(ctx, s.log, s.goalState.MachineName, "mkdir", "-p", filepath.Dir(destination)); err != nil {
			return fmt.Errorf("create NVIDIA VDPAU directory: %w", err)
		}
	}

	// Each discovered name is materialized as a regular file. Discovery
	// includes both aliases and their versioned targets, so the driver root
	// does not depend on symlinks resolving back into the host mount.
	if _, err := executil.MachineRun(ctx, s.log, s.goalState.MachineName, "cp", "-aL", lib.ContainerPath, destination); err != nil {
		return fmt.Errorf("copy NVIDIA library %s: %w", lib.ContainerPath, err)
	}

	s.log.Debug("copied NVIDIA driver library",
		slog.String("source", lib.ContainerPath),
		slog.String("destination", destination),
	)

	return nil
}

// copyNVIDIASMI adds the host-matched diagnostic binary to the prepared root.
// CDI generation uses --driver-root, so nvidia-ctk searches that root rather
// than the nspawn OCI filesystem for driver helper binaries. Including the
// host copy lets the resulting CDI spec expose a version-compatible nvidia-smi
// to GPU containers. It remains optional for compute-only CUDA workloads.
func (s *setupNVIDIA) copyNVIDIASMI(ctx context.Context, paths nvidiaDriverRootPaths) error {
	if s.goalState.Nvidia.NvidiaSMIPath == "" {
		return nil
	}

	source := filepath.Join(goalstates.NvidiaHostBinDir, filepath.Base(s.goalState.Nvidia.NvidiaSMIPath))

	destination := filepath.Join(paths.rootDir, "usr", "bin", "nvidia-smi")
	if _, err := executil.MachineRun(ctx, s.log, s.goalState.MachineName, "cp", "-L", source, destination); err != nil {
		return fmt.Errorf("copy nvidia-smi: %w", err)
	}

	s.log.Debug("copied nvidia-smi",
		slog.String("source", source),
		slog.String("destination", destination),
	)

	return nil
}

// copyNVIDIAIMEXBinaries adds the host-matched IMEX helpers to the prepared
// NVIDIA driver root. DRA ComputeDomain daemon pods run nvidia-imex from the
// driver root exposed at /run/nvidia/driver.
func (s *setupNVIDIA) copyNVIDIAIMEXBinaries(ctx context.Context, paths nvidiaDriverRootPaths) error {
	binaries := []struct {
		name string
		path string
	}{
		{name: "nvidia-imex", path: s.goalState.Nvidia.NvidiaIMEXPath},
		{name: "nvidia-imex-ctl", path: s.goalState.Nvidia.NvidiaIMEXCtlPath},
	}

	for _, binary := range binaries {
		if binary.path == "" {
			continue
		}

		source := filepath.Join(goalstates.NvidiaHostBinDir, filepath.Base(binary.path))

		destination := filepath.Join(paths.rootDir, "usr", "bin", binary.name)
		if _, err := executil.MachineRun(ctx, s.log, s.goalState.MachineName, "cp", "-L", source, destination); err != nil {
			return fmt.Errorf("copy %s: %w", binary.name, err)
		}

		s.log.Debug("copied NVIDIA IMEX binary",
			slog.String("source", source),
			slog.String("destination", destination),
		)
	}

	return nil
}

func (s *setupNVIDIA) createDriverRootLinks(ctx context.Context, paths nvidiaDriverRootPaths) error {
	machine := s.goalState.MachineName

	// Mirror the normal distribution multiarch layout under usr/lib. Relative
	// targets are required here so the links remain inside the driver root when
	// NVIDIA tooling mounts it at another path, such as /driver-root.
	multiarchSource := filepath.Join("..", "..", "lib", paths.libDirName)

	multiarchDestination := filepath.Join(paths.rootDir, "usr", "lib", paths.libDirName)
	if _, err := executil.MachineRun(ctx, s.log, machine,
		"ln", "-sfn", multiarchSource, multiarchDestination,
	); err != nil {
		return fmt.Errorf("link NVIDIA multiarch library directory: %w", err)
	}

	s.log.Debug("linked NVIDIA multiarch library directory",
		slog.String("source", multiarchSource),
		slog.String("destination", multiarchDestination),
	)

	i386Source := filepath.Join("..", "..", "lib", "i386-linux-gnu")

	i386Destination := filepath.Join(paths.rootDir, "usr", "lib", "i386-linux-gnu")
	if _, err := executil.MachineRun(ctx, s.log, machine,
		"ln", "-sfn", i386Source, i386Destination,
	); err != nil {
		return fmt.Errorf("link NVIDIA i386 library directory: %w", err)
	}

	s.log.Debug("linked NVIDIA i386 library directory",
		slog.String("source", i386Source),
		slog.String("destination", i386Destination),
	)

	// The NVIDIA update-ldcache hook invokes <driver-root>/sbin/ldconfig.
	// Reuse the machine's binary instead of copying it and its dependencies.
	ldconfigDestination := filepath.Join(paths.rootDir, "sbin", "ldconfig")
	if _, err := executil.MachineRun(ctx, s.log, machine,
		"ln", "-sfn", "/sbin/ldconfig", ldconfigDestination,
	); err != nil {
		return fmt.Errorf("link NVIDIA driver ldconfig: %w", err)
	}

	s.log.Debug("linked NVIDIA driver ldconfig",
		slog.String("source", "/sbin/ldconfig"),
		slog.String("destination", ldconfigDestination),
	)

	return nil
}

func (s *setupNVIDIA) generateDriverRootLinkerCache(ctx context.Context, paths nvidiaDriverRootPaths) error {
	// nvidia-ctk reads <driver-root>/etc/ld.so.cache to discover the libraries
	// available from the prepared root. Without this cache it warns that the
	// driver root has no linker cache and may fail to resolve SONAME aliases.
	// These absolute paths are interpreted inside the root passed to ldconfig
	// with -r; they do not refer to the nspawn machine's root filesystem.
	ldConfigDirs := []string{filepath.Join("/lib", paths.libDirName), filepath.Join("/usr/lib", paths.libDirName)}
	if len(s.goalState.Nvidia.I386LibDirMounts) > 0 {
		ldConfigDirs = append(ldConfigDirs, "/lib/i386-linux-gnu", "/usr/lib/i386-linux-gnu")
	}

	// Supply the output path as the shell's $0 and each linker directory as a
	// positional argument, avoiding interpolation of paths into the script.
	ldConfigPath := filepath.Join(paths.rootDir, "etc", "ld.so.conf")
	writeConfigArgs := []string{"sh", "-c", `printf '%s\n' "$@" > "$0"`, ldConfigPath}
	writeConfigArgs = append(writeConfigArgs, ldConfigDirs...)

	if _, err := executil.MachineRun(ctx, s.log, s.goalState.MachineName, writeConfigArgs...); err != nil {
		return fmt.Errorf("write NVIDIA driver linker configuration: %w", err)
	}

	s.log.Debug("wrote NVIDIA driver linker configuration", slog.String("path", ldConfigPath))

	// Build etc/ld.so.cache inside the driver root and let ldconfig establish
	// the standard linker aliases expected by NVIDIA tooling.
	if _, err := executil.MachineRun(ctx, s.log, s.goalState.MachineName, "ldconfig", "-r", paths.rootDir); err != nil {
		return fmt.Errorf("generate NVIDIA driver linker cache: %w", err)
	}

	s.log.Debug("generated NVIDIA driver linker cache",
		slog.String("path", filepath.Join(paths.rootDir, "etc", "ld.so.cache")),
	)

	return nil
}

func (s *setupNVIDIA) validateDriverRoot(ctx context.Context, paths nvidiaDriverRootPaths) error {
	// nvidia-ctk requires exact driver-version filenames for CUDA and NVML.
	// They must contain the library bytes rather than point back to a SONAME
	// alias, which nvidia-ctk can reject as an unexpected library version.
	for _, requiredPath := range requiredVersionedNVIDIALibraryPaths(paths.libDir, s.goalState.Nvidia.DriverVersion) {
		if _, err := executil.MachineRun(ctx, s.log, s.goalState.MachineName, "test", "-f", requiredPath); err != nil {
			return fmt.Errorf("required NVIDIA driver library %s is missing: %w", requiredPath, err)
		}

		if _, err := executil.MachineRun(ctx, s.log, s.goalState.MachineName, "test", "!", "-L", requiredPath); err != nil {
			return fmt.Errorf("required NVIDIA driver library %s is not a real file: %w", requiredPath, err)
		}
	}

	return nil
}

func requiredVersionedNVIDIALibraryPaths(driverLibDir, driverVersion string) []string {
	return []string{
		filepath.Join(driverLibDir, "libcuda.so."+driverVersion),
		filepath.Join(driverLibDir, "libnvidia-ml.so."+driverVersion),
	}
}

// nvidiaVersionedLibraryCopy describes a fallback copy that creates an actual
// versioned file rather than a symlink pointing back to a SONAME alias.
type nvidiaVersionedLibraryCopy struct {
	source              string
	relativeDestination string
}

func versionedNVIDIALibraryCopies(libs []goalstates.NvidiaLibMapping, driverVersion string) []nvidiaVersionedLibraryCopy {
	if driverVersion == "" {
		return nil
	}

	type candidate struct {
		mapping goalstates.NvidiaLibMapping
		score   int
	}

	existing := make(map[string]bool)
	candidates := make(map[string]candidate)

	for _, lib := range libs {
		name := filepath.Base(lib.HostPath)

		soIndex := strings.Index(name, ".so")
		if soIndex < 0 {
			continue
		}

		family := name[:soIndex+len(".so")]
		existing[name] = true

		suffix := strings.TrimPrefix(name, family)
		score := 3

		switch suffix {
		case ".1":
			score = 0
		case ".0", ".2":
			score = 1
		case "":
			score = 2
		}

		current, found := candidates[family]
		if !found || score < current.score {
			candidates[family] = candidate{mapping: lib, score: score}
		}
	}

	families := make([]string, 0, len(candidates))
	for family := range candidates {
		families = append(families, family)
	}

	sort.Strings(families)

	var copies []nvidiaVersionedLibraryCopy

	for _, family := range families {
		destinationName := family + "." + driverVersion
		if existing[destinationName] {
			continue
		}

		candidate := candidates[family].mapping
		relativeDestination := destinationName

		if filepath.Base(filepath.Dir(candidate.HostPath)) == "vdpau" {
			relativeDestination = filepath.Join("vdpau", destinationName)
		}

		copies = append(copies, nvidiaVersionedLibraryCopy{
			source:              candidate.ContainerPath,
			relativeDestination: relativeDestination,
		})
	}

	return copies
}

func (s *setupNVIDIA) generateCDISpec(ctx context.Context) error {
	machine := s.goalState.MachineName

	// Ensure the CDI spec directory exists in the machine rootfs (host-side path).
	cdiDir := filepath.Join(s.goalState.MachineDir, goalstates.CDISpecDir)
	if err := os.MkdirAll(cdiDir, 0o755); err != nil {
		return fmt.Errorf("create CDI spec directory %s: %w", cdiDir, err)
	}

	s.log.Info("generating CDI spec inside nspawn machine",
		slog.String("machine", machine),
		slog.String("output", goalstates.CDISpecFile),
	)

	// Force driver artifact discovery through the root assembled from the host
	// driver. Without --driver-root, nvidia-ctk inspects the nspawn OCI root and
	// can generate a spec from missing or mismatched image libraries and helper
	// binaries. Device nodes remain at their normal bind-mounted paths in the
	// nspawn machine, so their discovery root is still /.
	if _, err := executil.MachineRun(ctx, s.log, machine,
		goalstates.NvidiaCTKPath, "cdi", "generate",
		"--driver-root", goalstates.NvidiaDriverDir,
		"--dev-root", "/",
		"--output", goalstates.CDISpecFile,
		"--disable-hook", "all",
		"--enable-hook", "create-symlinks",
		"--enable-hook", "update-ldcache",
	); err != nil {
		return fmt.Errorf("nvidia-ctk cdi generate in %s: %w", machine, err)
	}

	s.log.Info("CDI spec generated", slog.String("path", goalstates.CDISpecFile))

	return nil
}
