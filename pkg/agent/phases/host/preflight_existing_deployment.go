// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package host

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/installstate"
	"github.com/Azure/unbounded/pkg/agent/preflight"
)

const checkExistingDeploymentName = "existing-deployment"

// CheckExistingDeployment verifies the host does not already contain
// node deployment artifacts. Bootstrap must start from a clean host;
// otherwise partial state from a prior run can be reused accidentally.
func CheckExistingDeployment(log *slog.Logger, hostPrefix string) preflight.Checker {
	return checkExistingDeployment(log, hostPrefix, defaultHostCheckDeps())
}

func checkExistingDeployment(log *slog.Logger, hostPrefix string, deps hostCheckDeps) preflight.Checker {
	return simpleHostChecker{name: checkExistingDeploymentName, check: func(ctx context.Context) []preflight.Result {
		// Artifacts belonging to this host's own unfinished installation are
		// not a reason to refuse. Bootstrap resumes over them, so reporting
		// them as errors here would fail the ExecStartPre of the very retry
		// that is meant to recover, and the check would contradict start.
		if resumable, reason := resumableInstallation(); resumable {
			return preflight.ResultsOK(checkExistingDeploymentName, "host deployment", reason)
		}

		results := existingDeploymentResults(ctx, log, hostPrefix, deps)
		if len(results) > 0 {
			return results
		}

		return preflight.ResultsOK(
			checkExistingDeploymentName,
			"host deployment",
			"no existing node deployment was detected",
		)
	}}
}

// resumableInstallation reports whether the host carries an unfinished
// installation that bootstrap is allowed to continue.
//
// Only the stage is consulted. Whether this particular invocation may resume it
// also depends on the machine name and config fingerprint, which start checks;
// preflight does not have a decision to make beyond "these artifacts have an
// owner that intends to come back for them".
func resumableInstallation() (bool, string) {
	rec, err := installstate.DefaultStore().Load()
	if err != nil || rec.Checkpoint == installstate.CheckpointComplete ||
		rec.Checkpoint == installstate.CheckpointResetting {
		return false, ""
	}

	return true, fmt.Sprintf(
		"an unfinished installation for machine %q is present and will be resumed",
		rec.MachineName,
	)
}

// EnsureNoExistingDeployment returns an error when the host already contains
// node deployment artifacts. It is used by start before any
// bootstrap task mutates host state.
func EnsureNoExistingDeployment(ctx context.Context, log *slog.Logger, hostPrefix string) error {
	return ensureNoExistingDeployment(ctx, log, hostPrefix, defaultHostCheckDeps())
}

func ensureNoExistingDeployment(ctx context.Context, log *slog.Logger, hostPrefix string, deps hostCheckDeps) error {
	results := existingDeploymentResults(ctx, log, hostPrefix, deps)
	if len(results) == 0 {
		return nil
	}

	var messages []string

	for _, result := range results {
		if result.Target != "" {
			messages = append(messages, fmt.Sprintf("%s (%s)", result.Message, result.Target))
		} else {
			messages = append(messages, result.Message)
		}
	}

	return fmt.Errorf(
		"existing node deployment detected; node reset is needed before running start again: %s",
		strings.Join(messages, "; "),
	)
}

func existingDeploymentResults(ctx context.Context, log *slog.Logger, hostPrefix string, deps hostCheckDeps) []preflight.Result {
	var results []preflight.Result

	for _, machineName := range []string{goalstates.NSpawnMachineKube1, goalstates.NSpawnMachineKube2} {
		log.Debug("checking for registered nspawn machine", "machine", machineName)

		if _, err := deps.outputCmd(ctx, log, "machinectl", "show", machineName); err == nil {
			results = append(results, existingDeploymentResult(
				"nspawn machine",
				machineName,
				"registered nspawn machine "+machineName,
			))
		}

		for _, artifact := range existingDeploymentMachineArtifacts(machineName) {
			results = appendExistingDeploymentArtifactResult(results, deps, artifact)
		}
	}

	for _, artifact := range existingDeploymentHostArtifacts(hostPrefix) {
		results = appendExistingDeploymentArtifactResult(results, deps, artifact)
	}

	return results
}

type existingDeploymentArtifact struct {
	description string
	path        string
}

func existingDeploymentMachineArtifacts(machineName string) []existingDeploymentArtifact {
	return []existingDeploymentArtifact{
		{
			description: "nspawn machine rootfs",
			path:        filepath.Join("/var/lib/machines", machineName),
		},
		{
			description: "nspawn configuration",
			path:        filepath.Join(goalstates.SystemdNSpawnDir, machineName+".nspawn"),
		},
		{
			description: "nspawn service override",
			path:        filepath.Join(goalstates.SystemdSystemDir, "systemd-nspawn@"+machineName+".service.d"),
		},
		{
			description: "bpffs mount path",
			path:        goalstates.BPFFSMountPath(machineName),
		},
		{
			description: "applied agent configuration",
			path:        goalstates.AppliedConfigPath(machineName),
		},
	}
}

func existingDeploymentHostArtifacts(hostPrefix string) []existingDeploymentArtifact {
	artifacts := []existingDeploymentArtifact{
		{
			description: "agent daemon unit",
			path:        filepath.Join(goalstates.SystemdSystemDir, goalstates.DaemonUnit),
		},
		{
			description: "agent daemon recovery unit",
			path:        filepath.Join(goalstates.SystemdSystemDir, goalstates.DaemonRecoveryUnit),
		},
	}

	// Check every prefix the recovery script could live under, not just the
	// configured one. A host provisioned under a different prefix is still a
	// dirty host, and missing it would let start clobber an existing
	// deployment.
	for _, prefix := range goalstates.KnownHostPrefixes(hostPrefix) {
		artifacts = append(artifacts, existingDeploymentArtifact{
			description: "agent daemon recovery script",
			path:        goalstates.ResolveHostPaths(prefix).DaemonRecoveryScript,
		})
	}

	return artifacts
}

func appendExistingDeploymentArtifactResult(
	results []preflight.Result,
	deps hostCheckDeps,
	artifact existingDeploymentArtifact,
) []preflight.Result {
	if _, err := deps.stat(artifact.path); err == nil {
		return append(results, existingDeploymentResult(artifact.description, artifact.path, artifact.description+" "+artifact.path))
	} else if !errors.Is(err, os.ErrNotExist) {
		return append(results, preflight.Error(
			checkExistingDeploymentName,
			artifact.path,
			"existing deployment artifact cannot be inspected: %s; node reset is needed before running preflight or start again",
			artifact.path,
		))
	}

	return results
}

func existingDeploymentResult(description, target, detail string) preflight.Result {
	return preflight.Error(
		checkExistingDeploymentName,
		target,
		"existing node deployment artifact detected (%s): %s; node reset is needed before running preflight or start again",
		description,
		detail,
	)
}
