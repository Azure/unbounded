// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signaladapter

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	signalv1 "github.com/Azure/unbounded/api/signal/v1alpha1"
)

func TestResolvePinnedCertificationTestArtifacts(t *testing.T) {
	t.Parallel()

	content := "#!/bin/sh\nexit 0\n"
	sum := sha256.Sum256([]byte(content))
	artifactDigest := "sha256:" + hex.EncodeToString(sum[:])
	contentDigest := "sha256:" + strings.Repeat("c", 64)
	test := signalv1.CertificationTest{
		TypeMeta: metav1.TypeMeta{
			APIVersion: signalv1.GroupVersion.String(),
			Kind:       "CertificationTest",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:       "inline-script",
			UID:        types.UID("test-uid"),
			Generation: 3,
		},
		Spec: signalv1.CertificationTestSpec{
			DisplayName: "Inline script",
			Owner:       "project-signal",
			Version:     "1",
			Artifacts: []signalv1.TestArtifact{{
				Name:    "check.sh",
				Content: content,
				Digest:  artifactDigest,
			}},
			Metrics: []signalv1.TestMetricDefinition{{
				Name:        "scriptResult",
				DisplayName: "Script result",
				Type:        signalv1.MetricValueTypeStatus,
				Required:    true,
			}},
			Topology: signalv1.TestTopology{MinParticipants: 1, MaxParticipants: 1},
			Driver: signalv1.CertificationTestDriver{
				Type: signalv1.TestDriverTypeScript,
				Script: &signalv1.ScriptTestDriver{
					Language:    signalv1.ScriptLanguageShell,
					Artifact:    "check.sh",
					RunnerImage: "example.invalid/runner@sha256:" + strings.Repeat("a", 64),
				},
			},
		},
		Status: signalv1.CertificationTestStatus{
			ObservedGeneration: 3,
			Trust: &signalv1.CertificationTestTrustStatus{
				State:         signalv1.CertificationTestTrustApproved,
				ContentDigest: contentDigest,
			},
		},
	}

	stage := resolvedStageForTest(t, test, contentDigest)
	runner := runnerWithCertificationTest(t, test)

	artifacts, err := runner.resolvePinnedCertificationTest(t.Context(), &stage)
	if err != nil {
		t.Fatalf("resolvePinnedCertificationTest() error = %v", err)
	}

	if len(artifacts) != 1 || artifacts[0] != test.Spec.Artifacts[0] {
		t.Fatalf("resolved artifacts = %#v, want %#v", artifacts, test.Spec.Artifacts)
	}

	tampered := *test.DeepCopy()
	tampered.Spec.Artifacts[0].Content += "echo changed\n"
	runner = runnerWithCertificationTest(t, tampered)

	if _, err := runner.resolvePinnedCertificationTest(t.Context(), &stage); err == nil {
		t.Fatal("resolvePinnedCertificationTest() accepted tampered artifact content")
	}
}

func resolvedStageForTest(
	t *testing.T,
	test signalv1.CertificationTest,
	contentDigest string,
) signalv1.ResolvedStagePlan {
	t.Helper()

	parameterDigest, err := digestJSON(test.Spec.Parameters)
	if err != nil {
		t.Fatal(err)
	}

	metricDigest, err := digestJSON(test.Spec.Metrics)
	if err != nil {
		t.Fatal(err)
	}

	securityDigest, err := digestJSON(test.Spec.Security)
	if err != nil {
		t.Fatal(err)
	}

	return signalv1.ResolvedStagePlan{
		Name: "inline-script",
		Test: signalv1.ResolvedCertificationTest{
			Identity: signalv1.TestIdentitySnapshot{
				Name:          test.Name,
				UID:           test.UID,
				Generation:    test.Generation,
				ContentDigest: contentDigest,
				Trust:         signalv1.CertificationTestTrustApproved,
			},
			ParameterSchemaDigest:      parameterDigest,
			MetricSchemaDigest:         metricDigest,
			SecurityRequirementsDigest: securityDigest,
			Metrics:                    append([]signalv1.TestMetricDefinition(nil), test.Spec.Metrics...),
			Topology:                   test.Spec.Topology,
			Driver:                     *test.Spec.Driver.DeepCopy(),
		},
	}
}

func runnerWithCertificationTest(
	t *testing.T,
	test signalv1.CertificationTest,
) Runner {
	t.Helper()

	content, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&test)
	if err != nil {
		t.Fatal(err)
	}

	return Runner{Dynamic: dynamicfake.NewSimpleDynamicClient(
		runtime.NewScheme(),
		&unstructured.Unstructured{Object: content},
	)}
}
