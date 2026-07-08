// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

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
	"github.com/Azure/unbounded/pkg/agent/preflight"
)

const checkExistingDeploymentName = "existing-deployment"

// CheckExistingDeployment verifies the host does not already contain
// unbounded-agent deployment artifacts. Bootstrap must start from a clean host;
// otherwise partial state from a prior run can be reused accidentally.
func CheckExistingDeployment(log *slog.Logger) preflight.Checker {
	return checkExistingDeployment(log, defaultHostCheckDeps())
}

func checkExistingDeployment(log *slog.Logger, deps hostCheckDeps) preflight.Checker {
	return simpleHostChecker{name: checkExistingDeploymentName, check: func(ctx context.Context) []preflight.Result {
		results := existingDeploymentResults(ctx, log, deps)
		if len(results) > 0 {
			return results
		}

		return preflight.ResultsOK(
			checkExistingDeploymentName,
			"host deployment",
			"no existing unbounded-agent deployment was detected",
		)
	}}
}

// EnsureNoExistingDeployment returns an error when the host already contains
// unbounded-agent deployment artifacts. It is used by start before any
// bootstrap task mutates host state.
func EnsureNoExistingDeployment(ctx context.Context, log *slog.Logger) error {
	return ensureNoExistingDeployment(ctx, log, defaultHostCheckDeps())
}

func ensureNoExistingDeployment(ctx context.Context, log *slog.Logger, deps hostCheckDeps) error {
	results := existingDeploymentResults(ctx, log, deps)
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
		"existing unbounded-agent deployment detected; run `unbounded-agent reset` before running start again: %s",
		strings.Join(messages, "; "),
	)
}

func existingDeploymentResults(ctx context.Context, log *slog.Logger, deps hostCheckDeps) []preflight.Result {
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
			path:        goalstates.DaemonRecoveryScriptPath,
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
			checkExistingDeploymentName,
			artifact.path,
			"existing deployment artifact cannot be inspected: %s; run `unbounded-agent reset` before running preflight or start again",
			artifact.path,
		))
	}

	return results
}

func existingDeploymentResult(description, target, detail string) preflight.Result {
	return preflight.Error(
		checkExistingDeploymentName,
		target,
		"existing unbounded-agent deployment artifact detected (%s): %s; run `unbounded-agent reset` before running preflight or start again",
		description,
		detail,
	)
}
