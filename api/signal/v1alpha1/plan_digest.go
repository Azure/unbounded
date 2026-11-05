// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"encoding/json"
	"errors"
	"fmt"
)

const ExecutionPlanDigestSchema = "signal.unbounded-cloud.io/execution-plan/v1"

type executionPlanDigestInput struct {
	Schema             string                       `json:"schema"`
	ParticipantCount   int32                        `json:"participantCount"`
	ScriptBundleDigest string                       `json:"scriptBundleDigest"`
	Checks             []CertificationCheckSnapshot `json:"checks"`
	Provenance         ExecutionPlanProvenance      `json:"provenance"`
}

// DeriveExecutionPlanDigest returns the canonical digest of the resolved
// execution semantics. Object identity is intentionally excluded so equivalent
// resolved plans have the same digest.
func DeriveExecutionPlanDigest(
	participantCount int32,
	scriptBundleDigest string,
	checks []CertificationCheckSnapshot,
	provenance ExecutionPlanProvenance,
) (string, error) {
	if participantCount < 1 {
		return "", errors.New("participant count must be positive")
	}

	if !IsSHA256Digest(scriptBundleDigest) {
		return "", errors.New("script bundle digest is invalid")
	}

	if len(checks) == 0 {
		return "", errors.New("execution plan must contain at least one check")
	}

	if !IsDigestPinnedImage(provenance.AdapterImage) {
		return "", errors.New("adapter image must be digest-pinned")
	}

	if !IsSHA256Digest(provenance.WorkflowTemplateDigest) {
		return "", errors.New("workflow template digest is invalid")
	}

	for index := range checks {
		if !IsSHA256Digest(checks[index].Script.Digest) {
			return "", fmt.Errorf("check %d script digest is invalid", index)
		}

		if !IsDigestPinnedImage(checks[index].RunnerImage) {
			return "", fmt.Errorf("check %d runner image must be digest-pinned", index)
		}
	}

	content, err := json.Marshal(executionPlanDigestInput{
		Schema:             ExecutionPlanDigestSchema,
		ParticipantCount:   participantCount,
		ScriptBundleDigest: scriptBundleDigest,
		Checks:             checks,
		Provenance:         provenance,
	})
	if err != nil {
		return "", fmt.Errorf("encode execution plan: %w", err)
	}

	return sha256Digest(content), nil
}
