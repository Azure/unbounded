// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signaladapter

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	signalv1 "github.com/Azure/unbounded/api/signal/v1alpha1"
)

func TestLegacyMeasurementsRedactRawIdentity(t *testing.T) {
	t.Parallel()

	measurements, err := legacyMeasurements(`{
		"nvidiaSmiAvailable": true,
		"observedGpuCount": 8,
		"driverVersionSet": "550.1",
		"deviceInventoryDigest": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"gpuUUID": "GPU-raw",
		"nodeName": "raw-node"
	}`)
	if err != nil {
		t.Fatalf("legacyMeasurements() error = %v", err)
	}

	names := make([]string, 0, len(measurements))
	for _, measurement := range measurements {
		names = append(names, measurement.Name)
	}

	joined := strings.Join(names, ",")
	if strings.Contains(joined, "gpuUUID") || strings.Contains(joined, "nodeName") {
		t.Fatalf("legacy measurements expose raw identities: %s", joined)
	}

	if !strings.Contains(joined, "nvidiaSmiAvailable") ||
		!strings.Contains(joined, "observedGpuCount") ||
		!strings.Contains(joined, "driverVersionSet") ||
		!strings.Contains(joined, "deviceInventoryDigest") {
		t.Fatalf("legacy measurements omitted approved GPU inventory metrics: %s", joined)
	}
}

func TestValidateWorkflowTemplateIdentityRequiresImmutableInput(t *testing.T) {
	t.Parallel()

	digest := testStageDigest("template")
	invocation := StageInvocation{Stage: signalv1.ResolvedStagePlan{
		Test: signalv1.ResolvedCertificationTest{
			Driver: signalv1.CertificationTestDriver{
				Type: signalv1.TestDriverTypeWorkflowTemplate,
				WorkflowTemplate: &signalv1.WorkflowTemplateTestDriver{
					Name:   "vllm-canary",
					Digest: digest,
				},
			},
		},
		ResolvedInputs: []signalv1.ResolvedInputIdentity{{
			Name:     "workflow",
			Kind:     signalv1.ResolvedInputKindWorkflowTemplate,
			State:    signalv1.ResolvedInputStateComplete,
			Resolved: "vllm-canary@42",
			Digest:   digest,
		}},
	}}
	template := &unstructured.Unstructured{}
	template.SetName("vllm-canary")
	template.SetResourceVersion("42")

	if err := validateWorkflowTemplateIdentity(invocation, template); err != nil {
		t.Fatalf("validateWorkflowTemplateIdentity() error = %v", err)
	}

	template.SetResourceVersion("43")

	if err := validateWorkflowTemplateIdentity(invocation, template); err == nil {
		t.Fatal("validateWorkflowTemplateIdentity() accepted a changed resource version")
	}
}
