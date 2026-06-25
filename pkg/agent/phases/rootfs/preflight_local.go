// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package rootfs

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/internal/utilio"
	"github.com/Azure/unbounded/pkg/agent/preflight"
)

const (
	checkNSpawnMachineProvisioningName = "nspawn-machine-provisioning"
)

type rootFSCheckDeps struct {
	stat       func(string) (fs.FileInfo, error)
	open       func(string) (*os.File, error)
	writeProbe func(string) error
}

func defaultRootFSCheckDeps() rootFSCheckDeps {
	return rootFSCheckDeps{
		stat:       os.Stat,
		open:       os.Open,
		writeProbe: utilio.ProbeWritableDir,
	}
}

type nspawnMachineProvisioningChecker struct {
	log  *slog.Logger
	gs   *goalstates.RootFS
	deps rootFSCheckDeps
}

// CheckNSpawnMachineProvisioning validates local host paths needed to provision
// and configure the nspawn machine rootfs.
func CheckNSpawnMachineProvisioning(log *slog.Logger, gs *goalstates.RootFS) preflight.Checker {
	return checkNSpawnMachineProvisioning(log, gs, defaultRootFSCheckDeps())
}

func checkNSpawnMachineProvisioning(log *slog.Logger, gs *goalstates.RootFS, deps rootFSCheckDeps) preflight.Checker {
	return nspawnMachineProvisioningChecker{log: log, gs: gs, deps: deps}
}

func (c nspawnMachineProvisioningChecker) Name() string {
	return checkNSpawnMachineProvisioningName
}

func (c nspawnMachineProvisioningChecker) Check(context.Context) []preflight.Result {
	if c.gs == nil || c.gs.MachineDir == "" {
		return preflight.ResultsError(
			checkNSpawnMachineProvisioningName,
			"nspawn machine provisioning",
			"machine directory is not configured",
		)
	}

	results := c.checkMachineDir()

	if result := c.checkCreatableDir(filepath.Dir(c.gs.MachineDir), "rootfs parent directory"); result != nil {
		results = append(results, result...)
	}

	for _, path := range []string{
		filepath.Dir(c.gs.NSpawnConfigFile),
		filepath.Dir(c.gs.ServiceOverrideFile),
	} {
		if result := c.checkCreatableDir(path, "nspawn provisioning path"); result != nil {
			results = append(results, result...)
		}
	}

	if len(results) > 0 {
		return results
	}

	return preflight.ResultsOK(
		checkNSpawnMachineProvisioningName,
		"nspawn machine provisioning",
		"nspawn machine provisioning paths are ready",
	)
}

func (c nspawnMachineProvisioningChecker) checkMachineDir() []preflight.Result {
	c.log.Debug("checking machine directory", "path", c.gs.MachineDir)
	var results []preflight.Result

	info, err := c.deps.stat(c.gs.MachineDir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		// Missing machine directory is fine if the parent/provisioning paths are writable.
		return nil
	case err != nil:
		return preflight.ResultsError(
			checkNSpawnMachineProvisioningName,
			"nspawn machine provisioning",
			"machine directory cannot be inspected: %s",
			c.gs.MachineDir,
		)
	case !info.IsDir():
		return preflight.ResultsError(
			checkNSpawnMachineProvisioningName,
			"nspawn machine provisioning",
			"machine directory path is not a directory: %s",
			c.gs.MachineDir,
		)
	}

	// The nspawn machine root needs traversal permissions so dbus inside the
	// container can operate correctly when reusing an existing rootfs.
	if info.Mode().Perm()&0o055 != 0o055 {
		results = append(results, preflight.Warning(
			checkNSpawnMachineProvisioningName,
			"nspawn machine provisioning",
			"machine directory permissions are too restrictive: %s",
			c.gs.MachineDir,
		))
	}

	empty, err := isDirEmpty(c.deps.open, c.gs.MachineDir)
	if err != nil {
		return append(results, preflight.Error(
			checkNSpawnMachineProvisioningName,
			"nspawn machine provisioning",
			"machine directory cannot be read: %s",
			c.gs.MachineDir,
		))
	}

	if !empty {
		// A populated machine directory is expected during rejoin or reuse of an
		// existing kube1/kube2 rootfs. Rootfs provisioning will skip bootstrap
		// rather than overwrite it.
		c.log.Debug("machine directory exists and is not empty", "path", c.gs.MachineDir)
	} else {
		c.log.Debug("machine directory exists and is empty", "path", c.gs.MachineDir)
	}

	return results
}

func (c nspawnMachineProvisioningChecker) checkCreatableDir(path, label string) []preflight.Result {
	c.log.Debug("checking "+label, "path", path)
	existing := nearestExistingParent(c.deps.stat, path)

	if err := c.deps.writeProbe(existing); err != nil {
		return preflight.ResultsError(
			checkNSpawnMachineProvisioningName,
			"nspawn machine provisioning",
			"%s cannot be created under: %s",
			label,
			existing,
		)
	}

	return nil
}

func nearestExistingParent(stat func(string) (fs.FileInfo, error), path string) string {
	for {
		if info, err := stat(path); err == nil && info.IsDir() {
			return path
		}

		parent := filepath.Dir(path)
		if parent == path {
			return path
		}

		path = parent
	}
}

func isDirEmpty(open func(string) (*os.File, error), dir string) (bool, error) {
	f, err := open(dir)
	if err != nil {
		return false, err
	}

	defer f.Close() //nolint:errcheck // best effort close

	_, err = f.Readdirnames(1)
	if errors.Is(err, io.EOF) {
		return true, nil
	}

	return false, err
}
