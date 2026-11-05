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
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/none"
	"github.com/Azure/unbounded/pkg/agent/preflight"
)

// CheckExistingDeploymentName identifies the clean-host check, which a
// resumed installation intentionally skips.
const CheckExistingDeploymentName = "existing-deployment"

// CheckExistingDeployment verifies the host does not already contain nspawn
// node deployment artifacts.
//
// Deprecated: use CheckExistingDeploymentFor. This wrapper is kept for one
// release and checks for the nspawn mode.
func CheckExistingDeployment(log *slog.Logger) preflight.Checker {
	return CheckExistingDeploymentFor(log, isolation.ModeNSpawn)
}

// CheckExistingDeploymentFor verifies the host does not already contain node
// deployment artifacts. Bootstrap must start from a clean host; otherwise
// partial state from a prior run can be reused accidentally. Modes other than
// nspawn also look for the artifacts of a host-native node.
func CheckExistingDeploymentFor(log *slog.Logger, mode isolation.Mode) preflight.Checker {
	return checkExistingDeployment(log, defaultHostCheckDeps(), mode)
}

func checkExistingDeployment(log *slog.Logger, deps hostCheckDeps, mode isolation.Mode) preflight.Checker {
	return simpleHostChecker{name: CheckExistingDeploymentName, check: func(ctx context.Context) []preflight.Result {
		results := existingDeploymentResultsFor(ctx, log, deps, mode)
		if len(results) > 0 {
			return results
		}

		return preflight.ResultsOK(
			CheckExistingDeploymentName,
			"host deployment",
			"no existing node deployment was detected",
		)
	}}
}

// EnsureNoExistingDeployment returns an error when the host already contains
// node deployment artifacts. It is used by start before any
// bootstrap task mutates host state.
func EnsureNoExistingDeployment(ctx context.Context, log *slog.Logger) error {
	return ensureNoExistingDeployment(ctx, log, defaultHostCheckDeps(), isolation.DefaultMode)
}

// EnsureNoExistingDeploymentFor is EnsureNoExistingDeployment for an isolation
// mode.
func EnsureNoExistingDeploymentFor(ctx context.Context, log *slog.Logger, mode isolation.Mode) error {
	return ensureNoExistingDeployment(ctx, log, defaultHostCheckDeps(), mode)
}

func ensureNoExistingDeployment(ctx context.Context, log *slog.Logger, deps hostCheckDeps, mode isolation.Mode) error {
	results := existingDeploymentResultsFor(ctx, log, deps, mode)
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

// existingDeploymentResultsFor adds the artifacts of a host-native node for
// modes other than nspawn. The nspawn list is unchanged: those paths are absent
// on a host-native node and the host-native paths are absent on an nspawn one.
func existingDeploymentResultsFor(ctx context.Context, log *slog.Logger, deps hostCheckDeps, mode isolation.Mode) []preflight.Result {
	results := baseExistingDeploymentResults(ctx, log, deps)

	if mode == "" || mode == isolation.ModeNSpawn {
		return results
	}

	for _, artifact := range existingDeploymentNoneArtifacts() {
		results = appendExistingDeploymentArtifactResult(results, deps, artifact)
	}

	return results
}

func existingDeploymentNoneArtifacts() []existingDeploymentArtifact {
	return []existingDeploymentArtifact{
		{description: "host-native node directory", path: none.DefaultBase},
		{description: "host-native containerd unit", path: filepath.Join(goalstates.SystemdSystemDir, none.UnitContainerd)},
		{description: "host-native kubelet unit", path: filepath.Join(goalstates.SystemdSystemDir, none.UnitKubelet)},
		{description: "host-native LocalDNS unit", path: filepath.Join(goalstates.SystemdSystemDir, none.UnitLocalDNS)},
	}
}

func baseExistingDeploymentResults(ctx context.Context, log *slog.Logger, deps hostCheckDeps) []preflight.Result {
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

	for _, artifact := range existingDeploymentHostArtifacts() {
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

func existingDeploymentHostArtifacts() []existingDeploymentArtifact {
	return []existingDeploymentArtifact{
		{
			description: "agent daemon unit",
			path:        filepath.Join(goalstates.SystemdSystemDir, goalstates.DaemonUnit),
		},
		{
			description: "agent daemon recovery unit",
			path:        filepath.Join(goalstates.SystemdSystemDir, goalstates.DaemonRecoveryUnit),
		},
		{
			description: "agent daemon recovery script",
			path:        goalstates.PlannedHostPaths().DaemonRecoveryScript,
		},
	}
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
			CheckExistingDeploymentName,
			artifact.path,
			"existing deployment artifact cannot be inspected: %s; node reset is needed before running preflight or start again",
			artifact.path,
		))
	}

	return results
}

func existingDeploymentResult(description, target, detail string) preflight.Result {
	return preflight.Error(
		CheckExistingDeploymentName,
		target,
		"existing node deployment artifact detected (%s): %s; node reset is needed before running preflight or start again",
		description,
		detail,
	)
}
