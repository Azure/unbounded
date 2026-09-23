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
	"github.com/Azure/unbounded/pkg/agent/preflight"
)

// CheckExistingDeploymentName identifies the clean-host check, which a
// resumed installation intentionally skips.
const CheckExistingDeploymentName = "existing-deployment"

// CheckExistingDeployment verifies the host does not already contain node
// deployment artifacts, assuming the default installation prefix.
//
// Deprecated: use CheckExistingDeploymentFor, which also checks the configured
// installation prefix.
func CheckExistingDeployment(log *slog.Logger) preflight.Checker {
	return CheckExistingDeploymentFor(log, "")
}

// CheckExistingDeploymentFor verifies the host does not already contain node
// deployment artifacts. Bootstrap must start from a clean host; otherwise
// partial state from a prior run can be reused accidentally. Artifacts are
// looked for under the given installation prefix and under the default.
func CheckExistingDeploymentFor(log *slog.Logger, prefix string) preflight.Checker {
	return checkExistingDeployment(log, defaultHostCheckDeps(), prefix)
}

func checkExistingDeployment(log *slog.Logger, deps hostCheckDeps, prefix string) preflight.Checker {
	return simpleHostChecker{name: CheckExistingDeploymentName, check: func(ctx context.Context) []preflight.Result {
		results := existingDeploymentResults(ctx, log, deps, prefix)
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
// node deployment artifacts, assuming the default installation prefix.
//
// Deprecated: use EnsureNoExistingDeploymentFor, which also checks the
// configured installation prefix.
func EnsureNoExistingDeployment(ctx context.Context, log *slog.Logger) error {
	return EnsureNoExistingDeploymentFor(ctx, log, "")
}

// EnsureNoExistingDeploymentFor returns an error when the host already contains
// node deployment artifacts under the given installation prefix or the default.
// It is used by start before any bootstrap task mutates host state.
func EnsureNoExistingDeploymentFor(ctx context.Context, log *slog.Logger, prefix string) error {
	return ensureNoExistingDeployment(ctx, log, defaultHostCheckDeps(), prefix)
}

func ensureNoExistingDeployment(ctx context.Context, log *slog.Logger, deps hostCheckDeps, prefix string) error {
	results := existingDeploymentResults(ctx, log, deps, prefix)
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

func existingDeploymentResults(ctx context.Context, log *slog.Logger, deps hostCheckDeps, prefix string) []preflight.Result {
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

	for _, artifact := range existingDeploymentHostArtifacts(prefix) {
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

// existingDeploymentHostArtifacts returns the host files whose presence means
// this host already carries a deployment.
//
// The recovery script is looked for under every prefix the host might hold one
// under, not just the configured one. A host provisioned under a different
// prefix is still a dirty host, and checking only the configured prefix would
// let bootstrap run on top of one, which is the state this check exists to
// refuse.
func existingDeploymentHostArtifacts(prefix string) []existingDeploymentArtifact {
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

	for _, candidate := range goalstates.MergeHostPrefixes(prefix) {
		artifacts = append(artifacts, existingDeploymentArtifact{
			description: "agent daemon recovery script",
			path:        goalstates.ResolveHostPaths(candidate).DaemonRecoveryScript,
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
