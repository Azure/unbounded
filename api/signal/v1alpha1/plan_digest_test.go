// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	testScriptBundleDigest   = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testScriptDigest         = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	testRunnerImage          = "example.invalid/runner@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	testAdapterImage         = "example.invalid/adapter@sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	testWorkflowTemplateHash = "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
)

func TestDeriveExecutionPlanDigest(t *testing.T) {
	t.Parallel()

	baseChecks := func() []CertificationCheckSnapshot {
		minDuration := metav1.Duration{Duration: 5 * time.Minute}
		maxSampleGap := metav1.Duration{Duration: 30 * time.Second}

		return []CertificationCheckSnapshot{{
			Script: ScriptSnapshot{
				Key:    "check.sh",
				Digest: testScriptDigest,
			},
			RunnerImage: testRunnerImage,
			Backend:     ExecutionBackendPod,
			WorkloadPolicy: WorkloadPolicy{
				HostNetwork: true,
			},
			BackendActiveDeadline: metav1.Duration{Duration: 10 * time.Minute},
			MinDuration:           &minDuration,
			Continuity: &ContinuityPolicy{
				MaxSampleGap:             &maxSampleGap,
				RequireSameBootID:        true,
				RequirePerDeviceCoverage: true,
			},
		}}
	}

	baseProvenance := func() ExecutionPlanProvenance {
		return ExecutionPlanProvenance{
			AdapterImage:           testAdapterImage,
			WorkflowTemplateDigest: testWorkflowTemplateHash,
		}
	}

	want, err := DeriveExecutionPlanDigest(2, testScriptBundleDigest, baseChecks(), baseProvenance())
	if err != nil {
		t.Fatalf("derive base plan digest: %v", err)
	}

	got, err := DeriveExecutionPlanDigest(2, testScriptBundleDigest, baseChecks(), baseProvenance())
	if err != nil {
		t.Fatalf("derive equivalent plan digest: %v", err)
	}

	if got != want {
		t.Fatalf("equivalent plan digest = %q, want %q", got, want)
	}

	tests := []struct {
		name         string
		participants int32
		bundle       string
		mutate       func([]CertificationCheckSnapshot, *ExecutionPlanProvenance)
	}{
		{
			name:         "participant count",
			participants: 3,
		},
		{
			name:   "script bundle",
			bundle: "sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
		},
		{
			name: "script digest",
			mutate: func(checks []CertificationCheckSnapshot, _ *ExecutionPlanProvenance) {
				checks[0].Script.Digest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
			},
		},
		{
			name: "runner image",
			mutate: func(checks []CertificationCheckSnapshot, _ *ExecutionPlanProvenance) {
				checks[0].RunnerImage = "example.invalid/runner@sha256:2222222222222222222222222222222222222222222222222222222222222222"
			},
		},
		{
			name: "workload policy",
			mutate: func(checks []CertificationCheckSnapshot, _ *ExecutionPlanProvenance) {
				checks[0].WorkloadPolicy.HostNetwork = false
			},
		},
		{
			name: "minimum duration",
			mutate: func(checks []CertificationCheckSnapshot, _ *ExecutionPlanProvenance) {
				checks[0].MinDuration = &metav1.Duration{Duration: 6 * time.Minute}
			},
		},
		{
			name: "continuity",
			mutate: func(checks []CertificationCheckSnapshot, _ *ExecutionPlanProvenance) {
				checks[0].Continuity.RequireSameBootID = false
			},
		},
		{
			name: "adapter image",
			mutate: func(_ []CertificationCheckSnapshot, provenance *ExecutionPlanProvenance) {
				provenance.AdapterImage = "example.invalid/adapter@sha256:3333333333333333333333333333333333333333333333333333333333333333"
			},
		},
		{
			name: "workflow template",
			mutate: func(_ []CertificationCheckSnapshot, provenance *ExecutionPlanProvenance) {
				provenance.WorkflowTemplateDigest = "sha256:4444444444444444444444444444444444444444444444444444444444444444"
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			participants := tt.participants
			if participants == 0 {
				participants = 2
			}

			bundle := tt.bundle
			if bundle == "" {
				bundle = testScriptBundleDigest
			}

			checks := baseChecks()

			provenance := baseProvenance()
			if tt.mutate != nil {
				tt.mutate(checks, &provenance)
			}

			digest, err := DeriveExecutionPlanDigest(participants, bundle, checks, provenance)
			if err != nil {
				t.Fatalf("derive changed plan digest: %v", err)
			}

			if digest == want {
				t.Fatalf("changed execution semantics retained digest %q", digest)
			}
		})
	}
}

func TestDeriveExecutionPlanDigestRejectsMutableInputs(t *testing.T) {
	t.Parallel()

	checks := []CertificationCheckSnapshot{{
		Script: ScriptSnapshot{
			Key:    "check.sh",
			Digest: testScriptDigest,
		},
		RunnerImage:           "example.invalid/runner:latest",
		Backend:               ExecutionBackendPod,
		BackendActiveDeadline: metav1.Duration{Duration: time.Minute},
	}}
	provenance := ExecutionPlanProvenance{
		AdapterImage:           testAdapterImage,
		WorkflowTemplateDigest: testWorkflowTemplateHash,
	}

	if _, err := DeriveExecutionPlanDigest(1, testScriptBundleDigest, checks, provenance); err == nil {
		t.Fatal("derive plan digest accepted an unpinned runner image")
	}

	checks[0].RunnerImage = testRunnerImage

	provenance.AdapterImage = "example.invalid/adapter:latest"
	if _, err := DeriveExecutionPlanDigest(1, testScriptBundleDigest, checks, provenance); err == nil {
		t.Fatal("derive plan digest accepted an unpinned adapter image")
	}
}

func TestDigestPinnedImageValidation(t *testing.T) {
	t.Parallel()

	for _, valid := range []string{
		testRunnerImage,
		"registry.example.invalid:5000/path/image@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	} {
		if !IsDigestPinnedImage(valid) {
			t.Fatalf("IsDigestPinnedImage(%q) = false, want true", valid)
		}
	}

	for _, invalid := range []string{
		"",
		"example.invalid/runner:latest",
		"example.invalid/runner@sha512:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		"example.invalid/runner@sha256:CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC",
	} {
		if IsDigestPinnedImage(invalid) {
			t.Fatalf("IsDigestPinnedImage(%q) = true, want false", invalid)
		}
	}
}
