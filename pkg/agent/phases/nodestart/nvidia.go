// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package nodestart

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

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
// The task performs three steps in sequence:
//  1. Creates symlinks in the container's standard library path pointing into
//     the bind-mounted /run/host-nvidia/ directories, then runs ldconfig.
//  2. Ensures the CDI spec directory exists.
//  3. Runs nvidia-ctk cdi generate inside the machine to produce the CDI spec
//     at /etc/cdi/nvidia.yaml. Most hooks are disabled because they interfere
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

		if _, err := executil.MachineRun(ctx, s.log, machine, "cp", "-aL", lib.ContainerPath, destination); err != nil {
			return fmt.Errorf("copy NVIDIA library %s: %w", lib.ContainerPath, err)
		}
	}

	for _, mount := range s.goalState.Nvidia.I386LibDirMounts {
		if _, err := executil.MachineRun(ctx, s.log, machine, "cp", "-aL", mount.ContainerDir+"/.", i386LibDir); err != nil {
			return fmt.Errorf("copy i386 NVIDIA libraries from %s: %w", mount.ContainerDir, err)
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

	return nil
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
