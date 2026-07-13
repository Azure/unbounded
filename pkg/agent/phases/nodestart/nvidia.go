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
	if !s.goalState.Containerd.NvidiaRuntime.Enabled || len(s.goalState.Nvidia.LibMappings) == 0 {
		s.log.Info("NVIDIA runtime not enabled or no host libraries found, skipping")
		return nil
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

func (s *setupNVIDIA) prepareDriverRoot(ctx context.Context) error {
	machine := s.goalState.MachineName
	driverDir := goalstates.NvidiaDriverDir
	driverLibDir := filepath.Join(driverDir, "lib", filepath.Base(s.goalState.Nvidia.ContainerLibDir))
	i386LibDir := filepath.Join(driverDir, "lib", "i386-linux-gnu")

	if s.goalState.Nvidia.DriverVersion == "" {
		return fmt.Errorf("cannot prepare NVIDIA driver root: active driver version was not detected")
	}

	if _, err := executil.MachineRun(ctx, s.log, machine,
		"rm", "-rf", driverDir,
	); err != nil {
		return fmt.Errorf("remove NVIDIA driver root: %w", err)
	}

	if _, err := executil.MachineRun(ctx, s.log, machine,
		"mkdir", "-p", driverLibDir, i386LibDir,
		filepath.Join(driverDir, "usr", "bin"),
		filepath.Join(driverDir, "usr", "lib"),
		filepath.Join(driverDir, "sbin"),
		filepath.Join(driverDir, "etc"),
	); err != nil {
		return fmt.Errorf("create NVIDIA driver root: %w", err)
	}

	for _, lib := range s.goalState.Nvidia.LibMappings {
		destination := filepath.Join(driverLibDir, filepath.Base(lib.ContainerPath))
		if filepath.Base(filepath.Dir(lib.HostPath)) == "vdpau" {
			destination = filepath.Join(driverLibDir, "vdpau", filepath.Base(lib.ContainerPath))
			if _, err := executil.MachineRun(ctx, s.log, machine, "mkdir", "-p", filepath.Dir(destination)); err != nil {
				return fmt.Errorf("create NVIDIA VDPAU directory: %w", err)
			}
		}

		// Each discovered name is materialized as a regular file. Discovery
		// includes both aliases and their versioned targets, so the driver root
		// does not depend on symlinks resolving back into the host mount.
		if _, err := executil.MachineRun(ctx, s.log, machine, "cp", "-aL", lib.ContainerPath, destination); err != nil {
			return fmt.Errorf("copy NVIDIA library %s: %w", lib.ContainerPath, err)
		}
	}

	// Some installations contain only .so and .so.1 files even after the
	// filesystem scan. Materialize real files under the active driver version
	// because nvidia-ctk requires those names for core libraries.
	for _, copy := range versionedNVIDIALibraryCopies(s.goalState.Nvidia.LibMappings, s.goalState.Nvidia.DriverVersion) {
		destination := filepath.Join(driverLibDir, copy.relativeDestination)
		if _, err := executil.MachineRun(ctx, s.log, machine, "mkdir", "-p", filepath.Dir(destination)); err != nil {
			return fmt.Errorf("create directory for versioned NVIDIA library: %w", err)
		}

		if _, err := executil.MachineRun(ctx, s.log, machine, "cp", "-aL", copy.source, destination); err != nil {
			return fmt.Errorf("create versioned NVIDIA library %s: %w", destination, err)
		}
	}

	if filepath.Base(s.goalState.Nvidia.ContainerLibDir) == "x86_64-linux-gnu" && len(s.goalState.Nvidia.I386LibMappings) == 0 {
		s.log.Warn("matching NVIDIA compat32 libraries were not found",
			slog.String("driverVersion", s.goalState.Nvidia.DriverVersion),
		)
	}

	for _, lib := range s.goalState.Nvidia.I386LibMappings {
		destination := filepath.Join(i386LibDir, filepath.Base(lib.ContainerPath))
		if filepath.Base(filepath.Dir(lib.HostPath)) == "vdpau" {
			destination = filepath.Join(i386LibDir, "vdpau", filepath.Base(lib.ContainerPath))
			if _, err := executil.MachineRun(ctx, s.log, machine, "mkdir", "-p", filepath.Dir(destination)); err != nil {
				return fmt.Errorf("create i386 NVIDIA VDPAU directory: %w", err)
			}
		}

		if _, err := executil.MachineRun(ctx, s.log, machine, "cp", "-aL", lib.ContainerPath, destination); err != nil {
			return fmt.Errorf("copy i386 NVIDIA library %s: %w", lib.ContainerPath, err)
		}
	}

	if s.goalState.Nvidia.NvidiaSMIPath != "" {
		if _, err := executil.MachineRun(ctx, s.log, machine,
			"cp", "-L", filepath.Join(goalstates.NvidiaHostBinDir, filepath.Base(s.goalState.Nvidia.NvidiaSMIPath)),
			filepath.Join(driverDir, "usr", "bin", "nvidia-smi"),
		); err != nil {
			return fmt.Errorf("copy nvidia-smi: %w", err)
		}
	}

	if _, err := executil.MachineRun(ctx, s.log, machine,
		"ln", "-sfn", filepath.Join("..", "..", "lib", filepath.Base(s.goalState.Nvidia.ContainerLibDir)),
		filepath.Join(driverDir, "usr", "lib", filepath.Base(s.goalState.Nvidia.ContainerLibDir)),
	); err != nil {
		return fmt.Errorf("link NVIDIA multiarch library directory: %w", err)
	}

	if _, err := executil.MachineRun(ctx, s.log, machine,
		"ln", "-sfn", filepath.Join("..", "..", "lib", "i386-linux-gnu"),
		filepath.Join(driverDir, "usr", "lib", "i386-linux-gnu"),
	); err != nil {
		return fmt.Errorf("link NVIDIA i386 library directory: %w", err)
	}

	if _, err := executil.MachineRun(ctx, s.log, machine,
		"ln", "-sfn", "/sbin/ldconfig", filepath.Join(driverDir, "sbin", "ldconfig"),
	); err != nil {
		return fmt.Errorf("link NVIDIA driver ldconfig: %w", err)
	}

	ldConfigPath := filepath.Join(driverDir, "etc", "ld.so.conf")
	libDirName := filepath.Base(s.goalState.Nvidia.ContainerLibDir)

	ldConfigDirs := []string{filepath.Join("/lib", libDirName), filepath.Join("/usr/lib", libDirName)}
	if len(s.goalState.Nvidia.I386LibDirMounts) > 0 {
		ldConfigDirs = append(ldConfigDirs, "/lib/i386-linux-gnu", "/usr/lib/i386-linux-gnu")
	}

	writeConfigArgs := []string{"sh", "-c", `printf '%s\n' "$@" > "$0"`, ldConfigPath}
	writeConfigArgs = append(writeConfigArgs, ldConfigDirs...)

	if _, err := executil.MachineRun(ctx, s.log, machine, writeConfigArgs...); err != nil {
		return fmt.Errorf("write NVIDIA driver linker configuration: %w", err)
	}

	if _, err := executil.MachineRun(ctx, s.log, machine, "ldconfig", "-r", driverDir); err != nil {
		return fmt.Errorf("generate NVIDIA driver linker cache: %w", err)
	}

	for _, requiredPath := range requiredVersionedNVIDIALibraryPaths(driverLibDir, s.goalState.Nvidia.DriverVersion) {
		if _, err := executil.MachineRun(ctx, s.log, machine, "test", "-f", requiredPath); err != nil {
			return fmt.Errorf("required NVIDIA driver library %s is missing: %w", requiredPath, err)
		}

		if _, err := executil.MachineRun(ctx, s.log, machine, "test", "!", "-L", requiredPath); err != nil {
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
		name := filepath.Base(lib.ContainerPath)

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
