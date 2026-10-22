// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signalmanifest_test

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
)

func TestUpstreamManifestsContainControllerAndGenericAdapter(t *testing.T) {
	t.Parallel()

	templates, err := filepath.Glob("*.yaml.tmpl")
	if err != nil {
		t.Fatal(err)
	}

	sort.Strings(templates)

	want := []string{
		"00-namespace.yaml.tmpl",
		"01-serviceaccounts.yaml.tmpl",
		"02-rbac.yaml.tmpl",
		"03-adapter-serviceaccount-rbac.yaml.tmpl",
		"04-adapter-workflowtemplate.yaml.tmpl",
		"06-deployment.yaml.tmpl",
	}
	if !reflect.DeepEqual(templates, want) {
		t.Fatalf("manifest templates = %v, want %v", templates, want)
	}

	for _, path := range []string{"01-serviceaccounts.yaml.tmpl", "02-rbac.yaml.tmpl"} {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}

		manifest := string(content)
		if strings.Contains(manifest, "project-signal-runner") ||
			strings.Contains(manifest, "kubeflow.org") {
			t.Fatalf("%s contains bundled runner surface", path)
		}
	}
}

func TestAdapterManifestIsGenericAndParticipantAware(t *testing.T) {
	t.Parallel()

	content, err := os.ReadFile("04-adapter-workflowtemplate.yaml.tmpl")
	if err != nil {
		t.Fatal(err)
	}

	manifest := string(content)
	for _, required := range []string{
		"participants-json",
		"participant-count",
		"request-node-name",
		"backend-resources",
		"backend-evidence",
		"/outputs/backend-evidence",
		"cleanup-failure",
		"/outputs/cleanup-failure",
		"backend-api-version",
		"backend-kind",
		"backend-active-deadline-seconds",
		"queue",
		"runtime-stage-name",
		"script-requires-runtime-stage",
		"workload-policy",
	} {
		if !strings.Contains(manifest, required) {
			t.Fatalf("adapter WorkflowTemplate does not contain %q", required)
		}
	}

	for _, forbidden := range []string{
		"project-signal-certification/",
		"REPLACE_WITH_",
		"internal-node",
	} {
		if strings.Contains(manifest, forbidden) {
			t.Fatalf("adapter WorkflowTemplate contains downstream-specific value %q", forbidden)
		}
	}
}

func TestAdapterTemplateResolvesGroupedTaskArgumentsThroughInputs(t *testing.T) {
	t.Parallel()

	content, err := os.ReadFile("04-adapter-workflowtemplate.yaml.tmpl")
	if err != nil {
		t.Fatal(err)
	}

	manifest := string(content)
	if !strings.Contains(manifest, "entrypoint: certification-legacy") {
		t.Fatal("adapter WorkflowTemplate does not preserve the legacy workflow entrypoint")
	}

	const certificationMarker = "    - name: certification\n      inputs:\n"
	certificationIndex := strings.LastIndex(manifest, certificationMarker)
	if certificationIndex < 0 {
		t.Fatal("adapter WorkflowTemplate does not declare certification template inputs")
	}

	certification := manifest[certificationIndex:]
	if strings.Contains(certification, "workflow.parameters.") {
		t.Fatal("grouped certification template references undeclared workflow parameters")
	}

	inputReference := regexp.MustCompile(`inputs\.parameters\.([a-z0-9-]+)`)
	for _, match := range inputReference.FindAllStringSubmatch(certification, -1) {
		declaration := "          - name: " + match[1]
		if !strings.Contains(certification, declaration) {
			t.Fatalf("certification template references undeclared input %q", match[1])
		}
	}

	legacy := manifest[:certificationIndex]
	for _, required := range []string{
		"value: \"{{`{{workflow.parameters.run-name}}`}}\"",
		"parameter: \"{{`{{steps.certification.outputs.parameters.verdict}}`}}\"",
		"parameter: \"{{`{{steps.certification.outputs.parameters.backend-evidence}}`}}\"",
	} {
		if !strings.Contains(legacy, required) {
			t.Fatalf("legacy adapter wrapper does not contain %q", required)
		}
	}
}

func TestAdapterRBACAllowsBackendEvidenceCapture(t *testing.T) {
	t.Parallel()

	content, err := os.ReadFile("03-adapter-serviceaccount-rbac.yaml.tmpl")
	if err != nil {
		t.Fatal(err)
	}

	manifest := string(content)
	for _, required := range []string{
		`resources: ["pods"]`,
		`resources: ["pods/log"]`,
		`apiGroups: ["batch"]`,
		`resources: ["jobs"]`,
		`verbs: ["get", "list"]`,
	} {
		if !strings.Contains(manifest, required) {
			t.Fatalf("adapter RBAC does not contain %q", required)
		}
	}
}

func TestSignalCRDsExposeGroupedRuntimeAndDeadlineContract(t *testing.T) {
	t.Parallel()

	for _, path := range []string{
		"crd/signal.unbounded-cloud.io_certificationprofiles.yaml",
		"crd/signal.unbounded-cloud.io_certificationruns.yaml",
	} {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}

		manifest := string(content)
		for _, required := range []string{
			"checks:",
			"maxItems: 15",
			"minItems: 15",
			"executionPolicy:",
			"participantRoles:",
			"topologyConstraints:",
			"resultBounds:",
			"backendActiveDeadline:",
			"runtimeStage:",
			"requiresRuntimeStage:",
			"every backendActiveDeadline must leave at least 60 seconds",
			"runtimeStage is required when the script requires a runtime",
			"maxAttempts must be exactly 1",
			"checks must contain unique IDs",
			"maxLength: 63",
		} {
			if !strings.Contains(manifest, required) {
				t.Fatalf("%s does not contain %q", path, required)
			}

			profileContent, err := os.ReadFile("crd/signal.unbounded-cloud.io_certificationprofiles.yaml")
			if err != nil {
				t.Fatal(err)
			}

			if !strings.Contains(string(profileContent), "recurring certification is not supported for grouped suites") {
				t.Fatal("CertificationProfile CRD does not reject grouped recurrence")
			}

			runContent, err := os.ReadFile("crd/signal.unbounded-cloud.io_certificationruns.yaml")
			if err != nil {
				t.Fatal(err)
			}

			for _, required := range []string{"backendEvidence:", "workflowNodeName:", "retryCount:", "notRun:"} {
				if !strings.Contains(string(runContent), required) {
					t.Fatalf("CertificationRun CRD does not contain per-check status field %q", required)
				}
			}
		}
	}
}

func TestCertificationRunCRDMakesGroupedSnapshotImmutable(t *testing.T) {
	t.Parallel()

	content, err := os.ReadFile("crd/signal.unbounded-cloud.io_certificationruns.yaml")
	if err != nil {
		t.Fatal(err)
	}

	manifest := string(content)
	requiredRules := []string{
		"checks are immutable",
		"participants are immutable",
		"participantRoles are immutable",
		"topologyConstraints are immutable",
		"executionPolicy is immutable",
		"workflow is immutable",
	}

	for _, rule := range requiredRules {
		if !strings.Contains(manifest, rule) {
			t.Fatalf("CertificationRun CRD does not contain rule %q", rule)
		}
	}
}

func TestCertificationRunCRDRequiresCanonicalDigests(t *testing.T) {
	t.Parallel()

	content, err := os.ReadFile("crd/signal.unbounded-cloud.io_certificationruns.yaml")
	if err != nil {
		t.Fatal(err)
	}

	const digestPattern = "pattern: ^sha256:[0-9a-f]{64}$"
	if count := strings.Count(string(content), digestPattern); count != 3 {
		t.Fatalf("canonical digest patterns = %d, want 3", count)
	}
}

func TestSignalCRDsExposeBoundedKueueTopologyPolicy(t *testing.T) {
	t.Parallel()

	for _, path := range []string{
		"crd/signal.unbounded-cloud.io_certificationprofiles.yaml",
		"crd/signal.unbounded-cloud.io_certificationruns.yaml",
	} {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}

		manifest := string(content)
		for _, required := range []string{
			"kueueTopology:",
			"requiredLevel:",
			"maxLength: 316",
			"minLength: 1",
		} {
			if !strings.Contains(manifest, required) {
				t.Fatalf("%s does not contain %q", path, required)
			}
		}
	}
}

func TestUpstreamDoesNotPublishCertificationPackage(t *testing.T) {
	t.Parallel()

	for _, path := range []string{
		"../../project-signal-certification",
		"../../project-signal-certification-runner",
	} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("%s must not be published upstream: %v", path, err)
		}
	}
}
