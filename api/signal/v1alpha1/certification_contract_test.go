// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	schemacel "k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apimachinery/pkg/util/yaml"
)

func TestCertificationKindsRegistered(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		t.Fatalf("add Signal types to scheme: %v", err)
	}

	for _, object := range []runtime.Object{
		&CertificationTest{},
		&CertificationTestList{},
		&CertificationSuite{},
		&CertificationSuiteList{},
		&CertificationPolicy{},
		&CertificationPolicyList{},
		&NodeCertification{},
		&NodeCertificationList{},
	} {
		kinds, _, err := scheme.ObjectKinds(object)
		if err != nil {
			t.Fatalf("look up registered kind for %T: %v", object, err)
		}

		if len(kinds) != 1 || kinds[0].GroupVersion() != GroupVersion {
			t.Fatalf("registered kinds for %T = %v, want Signal v1alpha1", object, kinds)
		}
	}
}

func TestCertificationSuiteExecutionRequirements(t *testing.T) {
	t.Parallel()

	crd := readCRD(t, "signal.unbounded-cloud.io_certificationsuites.yaml")
	check := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.
		Properties["spec"].
		Properties["checks"].
		Items.Schema

	for _, property := range []string{"minDuration", "continuity", "execution"} {
		if _, found := check.Properties[property]; !found {
			t.Fatalf("generated Suite check has no %s", property)
		}
	}

	for _, rule := range []string{
		"!has(self.minDuration) || duration(self.minDuration) <= duration(self.backendActiveDeadline)",
		"!has(self.continuity) || !has(self.continuity.maxSampleGap) || duration(self.continuity.maxSampleGap) <= duration(self.backendActiveDeadline)",
		"!has(self.minDuration) || !has(self.continuity) || !has(self.continuity.maxSampleGap) || duration(self.continuity.maxSampleGap) <= duration(self.minDuration)",
	} {
		if !hasValidationRule(check, rule) {
			t.Fatalf("generated Suite check has no %q validation", rule)
		}
	}

	execution := check.Properties["execution"]
	for _, property := range []string{
		"role",
		"mode",
		"coverageWindow",
		"iterationTimeout",
		"interval",
		"minExecutions",
		"maxGap",
	} {
		if _, found := execution.Properties[property]; !found {
			t.Fatalf("generated Suite execution has no %s", property)
		}
	}

	for _, rule := range []string{
		"self.role == 'Validation' || self.mode == 'RunOnce'",
		"self.mode != 'RunOnce' || (!has(self.coverageWindow) && !has(self.iterationTimeout) && !has(self.interval) && !has(self.minExecutions) && !has(self.maxGap))",
		"self.mode == 'RunOnce' || (has(self.coverageWindow) && has(self.iterationTimeout) && has(self.minExecutions) && has(self.maxGap))",
		"self.mode != 'Periodic' || has(self.interval)",
		"self.mode == 'Periodic' || !has(self.interval)",
		"!has(self.coverageWindow) || duration(self.coverageWindow).getMilliseconds() % 1000 == 0",
		"!has(self.iterationTimeout) || duration(self.iterationTimeout).getMilliseconds() % 1000 == 0",
		"!has(self.interval) || duration(self.interval).getMilliseconds() % 1000 == 0",
		"!has(self.maxGap) || duration(self.maxGap).getMilliseconds() % 1000 == 0",
		"!has(self.interval) || !has(self.iterationTimeout) || duration(self.iterationTimeout) <= duration(self.interval)",
		"!has(self.interval) || !has(self.maxGap) || duration(self.interval) <= duration(self.maxGap)",
		"!has(self.interval) || !has(self.coverageWindow) || !has(self.iterationTimeout) || !has(self.minExecutions) || duration(self.iterationTimeout).getMilliseconds() + duration(self.interval).getMilliseconds() * (self.minExecutions - 1) <= duration(self.coverageWindow).getMilliseconds()",
	} {
		if !hasValidationRule(&execution, rule) {
			t.Fatalf("generated Suite execution has no %q validation", rule)
		}
	}

	if !hasValidationRule(
		check,
		"!has(self.execution) || !has(self.execution.coverageWindow) || duration(self.execution.coverageWindow) <= duration(self.backendActiveDeadline)",
	) {
		t.Fatal("generated Suite check does not bound coverageWindow by backendActiveDeadline")
	}

	if !hasValidationRule(
		check,
		"!has(self.execution) || self.execution.mode == 'RunOnce' || duration(self.backendActiveDeadline).getMilliseconds() % 1000 == 0",
	) {
		t.Fatal("generated Suite check does not require whole-second repeated execution")
	}
}

func TestCertificationCheckListContract(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		file       string
		schemaPath []string
	}{
		{
			name:       "suite spec",
			file:       "signal.unbounded-cloud.io_certificationsuites.yaml",
			schemaPath: []string{"spec", "checks"},
		},
		{
			name:       "run spec",
			file:       "signal.unbounded-cloud.io_certificationruns.yaml",
			schemaPath: []string{"spec", "checks"},
		},
		{
			name:       "run status",
			file:       "signal.unbounded-cloud.io_certificationruns.yaml",
			schemaPath: []string{"status", "checks"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			crd := readCRD(t, tt.file)

			schema := crd.Spec.Versions[0].Schema.OpenAPIV3Schema
			for _, property := range tt.schemaPath {
				next, found := schema.Properties[property]
				if !found {
					t.Fatalf("generated CRD has no %q property", property)
				}

				schema = &next
			}

			if schema.MaxItems == nil || *schema.MaxItems != MaxCertificationChecks {
				t.Fatalf("maxItems = %v, want %d", schema.MaxItems, MaxCertificationChecks)
			}

			if schema.XListType == nil || *schema.XListType != "atomic" {
				t.Fatalf("x-kubernetes-list-type = %v, want atomic", schema.XListType)
			}
		})
	}
}

func TestCertificationTestContract(t *testing.T) {
	t.Parallel()

	crd := readCRD(t, "signal.unbounded-cloud.io_certificationtests.yaml")
	if crd.Spec.Scope != apiextensionsv1.ClusterScoped {
		t.Fatalf("CertificationTest scope = %s, want Cluster", crd.Spec.Scope)
	}

	root := crd.Spec.Versions[0].Schema.OpenAPIV3Schema
	spec := root.Properties["spec"]
	driver := spec.Properties["driver"]

	for _, driverType := range []string{`"Script"`, `"Container"`, `"WorkflowTemplate"`} {
		if !containsJSONEnum(driver.Properties["type"].Enum, driverType) {
			t.Fatalf("driver enum = %s, missing %s", driver.Properties["type"].Enum, driverType)
		}
	}

	for _, rule := range []string{
		"self.type != 'Script' || has(self.script)",
		"self.type == 'Script' || !has(self.script)",
		"self.type != 'Container' || has(self.container)",
		"self.type == 'Container' || !has(self.container)",
		"self.type != 'WorkflowTemplate' || has(self.workflowTemplate)",
		"self.type == 'WorkflowTemplate' || !has(self.workflowTemplate)",
	} {
		if !hasValidationRule(&driver, rule) {
			t.Fatalf("generated test driver has no %q validation", rule)
		}
	}

	script := driver.Properties["script"]
	if script.Properties["runnerImage"].Pattern != DigestPinnedImagePattern {
		t.Fatal("Script runner image is not digest-pinned")
	}

	container := driver.Properties["container"]
	if container.Properties["image"].Pattern != DigestPinnedImagePattern {
		t.Fatal("Container image is not digest-pinned")
	}

	if args := container.Properties["args"]; args.MaxItems == nil || *args.MaxItems != 8 ||
		args.Items.Schema.MaxLength == nil || *args.Items.Schema.MaxLength != 256 {
		t.Fatalf("container argument bounds = %v items of %v bytes, want 8 of 256", args.MaxItems, args.Items.Schema.MaxLength)
	}

	template := driver.Properties["workflowTemplate"]
	if template.Properties["digest"].Pattern != SHA256DigestPattern {
		t.Fatal("WorkflowTemplate digest is not pinned")
	}

	trust := root.Properties["status"].Properties["trust"]
	for _, state := range []string{`"Approved"`, `"Diagnostic"`, `"Rejected"`, `"Revoked"`} {
		if !containsJSONEnum(trust.Properties["state"].Enum, state) {
			t.Fatalf("trust enum = %s, missing %s", trust.Properties["state"].Enum, state)
		}
	}

	parameters := spec.Properties["parameters"]
	if parameters.MaxItems == nil || *parameters.MaxItems != MaxTestParameters {
		t.Fatalf("parameter maxItems = %v, want %d", parameters.MaxItems, MaxTestParameters)
	}

	if rule := "!has(self.artifacts) || self.artifacts.map(a, a.content.size()).sum() <= 327680"; !hasValidationRule(&spec, rule) {
		t.Fatalf("generated CertificationTest spec has no %q validation", rule)
	}

	typedValue := parameters.Items.Schema.Properties["default"]
	if len(typedValue.XValidations) != 1 {
		t.Fatalf("typed value validation count = %d, want 1", len(typedValue.XValidations))
	}

	metrics := spec.Properties["metrics"]
	if metrics.MaxItems == nil || *metrics.MaxItems != MaxTestMetrics {
		t.Fatalf("metric maxItems = %v, want %d", metrics.MaxItems, MaxTestMetrics)
	}

	metric := metrics.Items.Schema
	telemetry := metric.Properties["telemetry"]

	dimensions := telemetry.Properties["dimensions"]
	if dimensions.MaxItems == nil || *dimensions.MaxItems != MaxMetricDimensions {
		t.Fatalf("metric dimension maxItems = %v, want %d", dimensions.MaxItems, MaxMetricDimensions)
	}

	if maximum := telemetry.Properties["seriesLimit"].Maximum; maximum == nil ||
		*maximum != MaxMetricSeries {
		t.Fatalf("metric series maximum = %v, want %d", maximum, MaxMetricSeries)
	}

	if maximum := telemetry.Properties["sampleLimit"].Maximum; maximum == nil ||
		*maximum != MaxMetricSamples {
		t.Fatalf("metric sample maximum = %v, want %d", maximum, MaxMetricSamples)
	}

	if rule := "!has(self.telemetry) || self.type in ['Gauge', 'Ratio', 'Throughput', 'IOPS', 'Bandwidth', 'Latency']"; !hasValidationRule(metric, rule) {
		t.Fatalf("generated metric definition has no %q validation", rule)
	}

	roles := spec.Properties["topology"].Properties["roles"]
	if roles.MaxItems == nil || *roles.MaxItems != MaxTestRoles {
		t.Fatalf("role maxItems = %v, want %d", roles.MaxItems, MaxTestRoles)
	}
}

func TestCertificationTestTopologyCEL(t *testing.T) {
	t.Parallel()

	crd := readCRD(t, "signal.unbounded-cloud.io_certificationtests.yaml")
	test, _, _ := genericVLLMContractFixture(t)
	base := unstructuredObject(t, test)

	tests := []struct {
		name       string
		mutate     func(map[string]any)
		wantErrors bool
	}{
		{
			name: "topology absent",
			mutate: func(object map[string]any) {
				delete(object["spec"].(map[string]any), "topology")
			},
		},
		{
			name: "roles omitted",
			mutate: func(object map[string]any) {
				delete(object["spec"].(map[string]any)["topology"].(map[string]any), "roles")
			},
		},
		{
			name: "empty roles",
			mutate: func(object map[string]any) {
				object["spec"].(map[string]any)["topology"].(map[string]any)["roles"] = []any{}
			},
		},
		{
			name: "valid roles",
			mutate: func(object map[string]any) {
				object["spec"].(map[string]any)["topology"].(map[string]any)["roles"] = []any{
					map[string]any{
						"name":        "worker",
						"displayName": "Worker",
						"minCount":    int64(1),
						"maxCount":    int64(1),
					},
				}
			},
		},
		{
			name: "optional role minCount omitted",
			mutate: func(object map[string]any) {
				object["spec"].(map[string]any)["topology"].(map[string]any)["roles"] = []any{
					map[string]any{
						"name":        "observer",
						"displayName": "Observer",
						"maxCount":    int64(1),
					},
				}
			},
		},
		{
			name: "invalid role bounds",
			mutate: func(object map[string]any) {
				object["spec"].(map[string]any)["topology"].(map[string]any)["roles"] = []any{
					map[string]any{
						"name":        "worker",
						"displayName": "Worker",
						"minCount":    int64(2),
						"maxCount":    int64(1),
					},
				}
			},
			wantErrors: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			object := deepCopyUnstructured(t, base)
			tt.mutate(object)

			errors := validateCRDCEL(t, crd, object)
			if tt.wantErrors && len(errors) == 0 {
				t.Fatal("server-equivalent CEL accepted invalid topology")
			}

			if !tt.wantErrors && len(errors) != 0 {
				t.Fatalf("server-equivalent CEL rejected topology: %v", errors.ToAggregate())
			}
		})
	}
}

func TestCertificationRunTopologyCELAllowsOmittedRoles(t *testing.T) {
	t.Parallel()

	crd := readCRD(t, "signal.unbounded-cloud.io_certificationruns.yaml")
	_, _, run := genericVLLMContractFixture(t)

	t.Run("roles omitted", func(t *testing.T) {
		t.Parallel()

		object := unstructuredObject(t, run)
		stage := object["spec"].(map[string]any)["resolvedPlan"].(map[string]any)["phases"].([]any)[0].(map[string]any)["stages"].([]any)[0].(map[string]any)
		delete(stage["test"].(map[string]any)["topology"].(map[string]any), "roles")
		delete(stage, "participantRoles")

		if errors := validateCRDCEL(t, crd, object); len(errors) != 0 {
			t.Fatalf("server-equivalent CEL rejected resolved stage without roles: %v", errors.ToAggregate())
		}
	})

	t.Run("optional role minCount omitted", func(t *testing.T) {
		t.Parallel()

		object := unstructuredObject(t, run)
		stage := object["spec"].(map[string]any)["resolvedPlan"].(map[string]any)["phases"].([]any)[0].(map[string]any)["stages"].([]any)[0].(map[string]any)
		topology := stage["test"].(map[string]any)["topology"].(map[string]any)
		topology["roles"] = []any{
			map[string]any{
				"name":        "observer",
				"displayName": "Observer",
				"maxCount":    int64(1),
			},
		}
		stage["participantRoles"] = []any{
			map[string]any{
				"role":               "observer",
				"participantIndexes": []any{int64(0)},
			},
		}

		if errors := validateCRDCEL(t, crd, object); len(errors) != 0 {
			t.Fatalf("server-equivalent CEL rejected optional role without minCount: %v", errors.ToAggregate())
		}
	})
}

func TestCertificationRunUpdateCELAllowsOmittedLegacyFields(t *testing.T) {
	t.Parallel()

	crd := readCRD(t, "signal.unbounded-cloud.io_certificationruns.yaml")
	_, _, run := genericVLLMContractFixture(t)
	base := unstructuredObject(t, run)

	for _, tt := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{
			name: "metadata update",
			mutate: func(object map[string]any) {
				object["metadata"].(map[string]any)["labels"] = map[string]any{"example": "updated"}
			},
		},
		{
			name: "status update",
			mutate: func(object map[string]any) {
				object["status"] = map[string]any{"phase": string(RunPhasePending)}
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			oldObject := deepCopyUnstructured(t, base)
			object := deepCopyUnstructured(t, base)
			tt.mutate(object)

			if errors := validateCRDCELUpdate(t, crd, object, oldObject); len(errors) != 0 {
				t.Fatalf("server-equivalent CEL rejected resolved Run update with legacy fields omitted: %v", errors.ToAggregate())
			}
		})
	}
}

func TestResourceCELAllowsResourcesOmitted(t *testing.T) {
	t.Parallel()

	t.Run("CertificationProfile environment", func(t *testing.T) {
		t.Parallel()

		crd := readCRD(t, "signal.unbounded-cloud.io_certificationprofiles.yaml")
		object := map[string]any{
			"apiVersion": GroupVersion.String(),
			"kind":       "CertificationProfile",
			"metadata": map[string]any{
				"name": "minimal",
			},
			"spec": map[string]any{
				"participants": map[string]any{
					"count": int64(1),
					"selector": map[string]any{
						"matchLabels": map[string]any{"example": "participant"},
					},
				},
				"environment": map[string]any{
					"namespace": "default",
				},
			},
		}

		if errors := validateCRDCEL(t, crd, object); len(errors) != 0 {
			t.Fatalf("server-equivalent CEL rejected Profile environment without resources: %v", errors.ToAggregate())
		}
	})

	t.Run("CertificationTest container", func(t *testing.T) {
		t.Parallel()

		crd := readCRD(t, "signal.unbounded-cloud.io_certificationtests.yaml")
		object := map[string]any{
			"apiVersion": GroupVersion.String(),
			"kind":       "CertificationTest",
			"metadata": map[string]any{
				"name": "minimal",
			},
			"spec": map[string]any{
				"topology": map[string]any{
					"minParticipants": int64(1),
					"maxParticipants": int64(1),
				},
				"driver": map[string]any{
					"type": "Container",
					"container": map[string]any{
						"image": fixtureImageA,
					},
				},
			},
		}

		if errors := validateCRDCEL(t, crd, object); len(errors) != 0 {
			t.Fatalf("server-equivalent CEL rejected Container driver without resources: %v", errors.ToAggregate())
		}
	})

	t.Run("CertificationTest script", func(t *testing.T) {
		t.Parallel()

		crd := readCRD(t, "signal.unbounded-cloud.io_certificationtests.yaml")
		object := map[string]any{
			"apiVersion": GroupVersion.String(),
			"kind":       "CertificationTest",
			"metadata": map[string]any{
				"name": "minimal",
			},
			"spec": map[string]any{
				"topology": map[string]any{
					"minParticipants": int64(1),
					"maxParticipants": int64(1),
				},
				"artifacts": []any{map[string]any{
					"name":    "check.sh",
					"content": "exit 0\n",
					"digest":  "sha256:" + strings.Repeat("a", 64),
				}},
				"driver": map[string]any{
					"type": "Script",
					"script": map[string]any{
						"language":    "Shell",
						"artifact":    "check.sh",
						"runnerImage": fixtureImageA,
					},
				},
			},
		}

		if errors := validateCRDCEL(t, crd, object); len(errors) != 0 {
			t.Fatalf("server-equivalent CEL rejected Script driver without resources: %v", errors.ToAggregate())
		}
	})

	t.Run("CertificationRun environment", func(t *testing.T) {
		t.Parallel()

		crd := readCRD(t, "signal.unbounded-cloud.io_certificationruns.yaml")
		_, _, run := genericVLLMContractFixture(t)
		object := unstructuredObject(t, run)
		delete(object["spec"].(map[string]any)["environment"].(map[string]any), "resources")

		if errors := validateCRDCEL(t, crd, object); len(errors) != 0 {
			t.Fatalf("server-equivalent CEL rejected resolved environment without resources: %v", errors.ToAggregate())
		}
	})

	t.Run("CertificationRun script", func(t *testing.T) {
		t.Parallel()

		crd := readCRD(t, "signal.unbounded-cloud.io_certificationruns.yaml")
		_, _, run := genericVLLMContractFixture(t)
		object := unstructuredObject(t, run)
		stages := object["spec"].(map[string]any)["resolvedPlan"].(map[string]any)["phases"].([]any)[0].(map[string]any)["stages"].([]any)
		stage := stages[0].(map[string]any)
		stage["test"].(map[string]any)["driver"] = map[string]any{
			"type": "Script",
			"script": map[string]any{
				"language":    "Shell",
				"artifact":    "check.sh",
				"runnerImage": fixtureImageA,
			},
		}

		if errors := validateCRDCEL(t, crd, object); len(errors) != 0 {
			t.Fatalf("server-equivalent CEL rejected resolved Script driver without resources: %v", errors.ToAggregate())
		}
	})
}

func TestCertificationSuitePhaseContract(t *testing.T) {
	t.Parallel()

	crd := readCRD(t, "signal.unbounded-cloud.io_certificationsuites.yaml")
	spec := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]

	for _, rule := range []string{
		"(has(self.phases) && size(self.phases) > 0) != (has(self.checks) && size(self.checks) > 0)",
		"!has(self.checks) || size(self.checks) == 0 || has(self.scriptBundle)",
		"!has(self.phases) || size(self.phases) == 0 || !has(self.scriptBundle)",
		"!has(self.phases) || self.phases.map(x, x.stages.size()).sum() <= 32",
		"!has(self.phases) || self.phases.filter(x, x.role == 'Startup').size() <= 1",
		"!has(self.phases) || self.phases.filter(x, x.role == 'Terminal').size() <= 1",
	} {
		if !hasValidationRule(&spec, rule) {
			t.Fatalf("Suite spec has no %q migration rule", rule)
		}
	}

	phases := spec.Properties["phases"]
	if phases.MaxItems == nil || *phases.MaxItems != 16 {
		t.Fatalf("phase maxItems = %v, want 16", phases.MaxItems)
	}

	if phases.XListType == nil || *phases.XListType != "atomic" {
		t.Fatalf("phase list type = %v, want atomic", phases.XListType)
	}

	phase := phases.Items.Schema
	if !hasValidationRule(
		phase,
		"self.role == 'Validation' || self.stages.all(x, x.schedule.mode == 'RunOnce')",
	) {
		t.Fatal("Startup and Terminal phases do not require RunOnce stages")
	}

	phaseStages := phase.Properties["stages"]
	if phaseStages.MaxItems == nil || *phaseStages.MaxItems != MaxCertificationStages {
		t.Fatalf("phase stage maxItems = %v, want %d", phaseStages.MaxItems, MaxCertificationStages)
	}

	stage := phaseStages.Items.Schema
	for _, property := range []string{
		"name",
		"displayName",
		"test",
		"parameters",
		"participantRoles",
		"schedule",
		"retry",
		"optional",
		"assertions",
		"perDeviceCoverageInterval",
	} {
		if _, found := stage.Properties[property]; !found {
			t.Fatalf("generated Suite stage has no %s", property)
		}
	}

	stringValue := stage.Properties["parameters"].
		Items.Schema.
		Properties["value"].
		Properties["stringValue"]
	if stringValue.MaxLength == nil || *stringValue.MaxLength != MaxTypedStringLength {
		t.Fatalf("stage parameter string maxLength = %v, want %d", stringValue.MaxLength, MaxTypedStringLength)
	}

	schedule := stage.Properties["schedule"]
	for _, rule := range []string{
		"self.mode != 'RunOnce' || (!has(self.coverageWindow) && !has(self.interval) && !has(self.minExecutions) && !has(self.maxGap))",
		"self.mode == 'RunOnce' || (has(self.coverageWindow) && has(self.minExecutions) && has(self.maxGap))",
		"self.mode != 'Periodic' || has(self.interval)",
		"self.mode == 'Periodic' || !has(self.interval)",
		"duration(self.invocationTimeout).getMilliseconds() % 1000 == 0",
		"!has(self.coverageWindow) || duration(self.coverageWindow).getMilliseconds() % 1000 == 0",
		"!has(self.interval) || duration(self.interval).getMilliseconds() % 1000 == 0",
		"!has(self.maxGap) || duration(self.maxGap).getMilliseconds() % 1000 == 0",
		"!has(self.interval) || !has(self.coverageWindow) || !has(self.minExecutions) || duration(self.invocationTimeout).getMilliseconds() + duration(self.interval).getMilliseconds() * (self.minExecutions - 1) <= duration(self.coverageWindow).getMilliseconds()",
	} {
		if !hasValidationRule(&schedule, rule) {
			t.Fatalf("generated stage schedule has no %q validation", rule)
		}
	}
}

func TestCertificationSuiteWorkloadPathBounds(t *testing.T) {
	t.Parallel()

	crd := readCRD(t, "signal.unbounded-cloud.io_certificationsuites.yaml")
	hostPath := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.
		Properties["spec"].
		Properties["checks"].
		Items.Schema.
		Properties["workloadPolicy"].
		Properties["hostPaths"].
		Items.Schema

	for _, property := range []string{"mountPath", "path"} {
		maxLength := hostPath.Properties[property].MaxLength
		if maxLength == nil || *maxLength != MaxWorkloadPathLength {
			t.Fatalf("%s maxLength = %v, want %d", property, maxLength, MaxWorkloadPathLength)
		}
	}

	for _, rule := range []string{
		"self.mountPath != '/scripts' && !self.mountPath.startsWith('/scripts/') && self.mountPath != '/results' && !self.mountPath.startsWith('/results/')",
		"(self.path == '/proc/cpuinfo' || self.path == '/proc/meminfo' || self.path == '/proc/version' || self.path == '/proc/sys/kernel/random/boot_id' || self.path == '/sys' || self.path.startsWith('/sys/') || self.path == '/var/lib/project-signal' || self.path.startsWith('/var/lib/project-signal/') || self.path == '/var/log' || self.path == '/run/log' || self.path == '/etc/machine-id') && !self.path.contains('//') && !self.path.contains('/./') && !self.path.contains('/../') && !self.path.endsWith('/.') && !self.path.endsWith('/..')",
		"!(self.path.startsWith('/proc/') || self.path == '/sys' || self.path.startsWith('/sys/') || self.path == '/var/log' || self.path == '/run/log' || self.path == '/etc/machine-id') || self.readOnly",
	} {
		if !hasValidationRule(hostPath, rule) {
			t.Fatalf("generated host path has no %q validation", rule)
		}
	}
}

func TestCertificationProfileReferencesSuite(t *testing.T) {
	t.Parallel()

	crd := readCRD(t, "signal.unbounded-cloud.io_certificationprofiles.yaml")

	spec := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]

	suite, found := spec.Properties["suite"]
	if !found {
		t.Fatal("generated Profile CRD has no suite reference")
	}

	if _, found := suite.Properties["name"]; !found {
		t.Fatal("generated Profile suite reference has no name")
	}

	if !contains(spec.Required, "suite") {
		t.Fatal("generated Profile CRD does not require suite")
	}

	for _, removed := range []string{"checks", "scriptBundle"} {
		if _, found := spec.Properties[removed]; found {
			t.Fatalf("generated Profile CRD still owns %s", removed)
		}
	}

	for _, property := range []string{"participantRoles", "environment"} {
		if _, found := spec.Properties[property]; !found {
			t.Fatalf("generated Profile CRD has no %s", property)
		}
	}

	if !hasValidationRule(
		&spec,
		"!has(self.participantRoles) || size(self.participantRoles) == 0 || self.participantRoles.map(x, x.count).sum() == self.participants.count",
	) {
		t.Fatal("Profile participant role counts are not bound to participants.count")
	}
}

func TestCertificationRunPinsSuiteAndRemainsImmutable(t *testing.T) {
	t.Parallel()

	crd := readCRD(t, "signal.unbounded-cloud.io_certificationruns.yaml")

	spec := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]

	suite, found := spec.Properties["suite"]
	if !found {
		t.Fatal("generated Run CRD has no pinned suite")
	}

	for _, property := range []string{"name", "uid", "generation"} {
		if _, found := suite.Properties[property]; !found {
			t.Fatalf("generated Run pinned suite has no %s", property)
		}

		if !contains(suite.Required, property) {
			t.Fatalf("generated Run pinned suite does not require %s", property)
		}
	}

	if !contains(spec.Required, "suite") {
		t.Fatal("generated Run CRD does not require suite")
	}

	requiredRules := map[string]bool{
		"self.profile == oldSelf.profile": false,
		"self.suite == oldSelf.suite":     false,
		"has(self.scriptBundle) == has(oldSelf.scriptBundle) && (!has(self.scriptBundle) || self.scriptBundle == oldSelf.scriptBundle)":                                                                                                                                        false,
		"has(self.checks) == has(oldSelf.checks) && (!has(self.checks) || self.checks == oldSelf.checks)":                                                                                                                                                                      false,
		"has(self.environment) == has(oldSelf.environment) && (!has(self.environment) || self.environment == oldSelf.environment)":                                                                                                                                             false,
		"has(self.resolvedPlan) == has(oldSelf.resolvedPlan) && (!has(self.resolvedPlan) || self.resolvedPlan == oldSelf.resolvedPlan)":                                                                                                                                        false,
		"(has(self.resolvedPlan) && size(self.resolvedPlan.phases) > 0) != (has(self.checks) && size(self.checks) > 0)":                                                                                                                                                        false,
		"!has(self.resolvedPlan) || (has(self.planDigest) && has(self.planProvenance) && has(self.planProvenance.controllerImage))":                                                                                                                                            false,
		"!has(self.resolvedPlan) || self.resolvedPlan.phases.all(p, p.stages.all(s, !has(s.participantRoles) || s.participantRoles.all(r, r.participantIndexes.all(i, i < size(self.participants)))))":                                                                         false,
		"!has(self.resolvedPlan) || self.resolvedPlan.phases.all(p, p.stages.all(s, (has(s.participantRoles) && size(s.participantRoles) > 0) || (size(self.participants) >= s.test.topology.minParticipants && size(self.participants) <= s.test.topology.maxParticipants)))": false,
		"has(self.planDigest) == has(oldSelf.planDigest) && (!has(self.planDigest) || self.planDigest == oldSelf.planDigest)":                                                                                                                                                  false,
		"has(self.planProvenance) == has(oldSelf.planProvenance) && (!has(self.planProvenance) || self.planProvenance == oldSelf.planProvenance)":                                                                                                                              false,
	}

	for _, validation := range spec.XValidations {
		if _, found := requiredRules[validation.Rule]; found {
			requiredRules[validation.Rule] = true
		}
	}

	for rule, found := range requiredRules {
		if !found {
			t.Fatalf("generated Run CRD has no %q immutability rule", rule)
		}
	}
}

func TestCertificationRunPlanAndEvidenceContract(t *testing.T) {
	t.Parallel()

	crd := readCRD(t, "signal.unbounded-cloud.io_certificationruns.yaml")
	root := crd.Spec.Versions[0].Schema.OpenAPIV3Schema
	spec := root.Properties["spec"]

	planDigest := spec.Properties["planDigest"]
	if planDigest.Pattern != SHA256DigestPattern {
		t.Fatalf("planDigest pattern = %q, want %q", planDigest.Pattern, SHA256DigestPattern)
	}

	if contains(spec.Required, "planDigest") || contains(spec.Required, "planProvenance") {
		t.Fatal("new plan fields must remain optional for backward decoding")
	}

	provenance := spec.Properties["planProvenance"]

	adapterImage := provenance.Properties["adapterImage"]
	if adapterImage.Pattern != DigestPinnedImagePattern {
		t.Fatalf("adapter image pattern = %q, want %q", adapterImage.Pattern, DigestPinnedImagePattern)
	}

	resolvedPlan := spec.Properties["resolvedPlan"]
	if _, found := resolvedPlan.Properties["phases"]; !found {
		t.Fatal("Run resolvedPlan has no phases")
	}

	resolvedStage := resolvedPlan.Properties["phases"].Items.Schema.
		Properties["stages"].Items.Schema
	for _, rule := range []string{
		"!self.qualificationEligible || (self.test.identity.trust == 'Approved' && (!has(self.diagnosticOnly) || !self.diagnosticOnly))",
		"!self.qualificationEligible || !has(self.resolvedInputs) || self.resolvedInputs.all(i, i.state == 'Complete')",
		"!has(self.resolvedInputs) || self.resolvedInputs.all(i, i.state != 'Incomplete') || (has(self.diagnosticOnly) && self.diagnosticOnly && !self.qualificationEligible)",
		"self.test.identity.trust == 'Approved' || (has(self.diagnosticOnly) && self.diagnosticOnly && !self.qualificationEligible)",
		"self.test.identity.trust != 'Rejected' && self.test.identity.trust != 'Revoked'",
		"!has(self.perDeviceCoverageInterval) || self.schedule.mode != 'RunOnce'",
		"!has(self.schedule.coverageWindow) || !has(self.perDeviceCoverageInterval) || duration(self.perDeviceCoverageInterval) <= duration(self.schedule.coverageWindow)",
	} {
		if !hasValidationRule(resolvedStage, rule) {
			t.Fatalf("resolved stage has no %q trust validation", rule)
		}
	}

	resolvedInputs := resolvedStage.Properties["resolvedInputs"]
	if resolvedInputs.MaxItems == nil || *resolvedInputs.MaxItems != 8 {
		t.Fatalf("resolved input maxItems = %v, want 8", resolvedInputs.MaxItems)
	}

	if resolvedInputs.Items.Schema.Properties["digest"].Pattern != SHA256DigestPattern {
		t.Fatal("resolved input digest is not SHA-256 constrained")
	}

	for _, rule := range []string{
		"self.state != 'Complete' || (has(self.resolved) && size(self.resolved.trim()) > 0 && has(self.digest))",
		"self.state != 'Incomplete' || (has(self.incompleteReason) && size(self.incompleteReason.trim()) > 0 && !has(self.digest))",
	} {
		if !hasValidationRule(resolvedInputs.Items.Schema, rule) {
			t.Fatalf("resolved input has no %q completeness rule", rule)
		}
	}

	resolvedRole := resolvedStage.Properties["participantRoles"].Items.Schema

	indexes := resolvedRole.Properties["participantIndexes"]
	if indexes.MinItems == nil || *indexes.MinItems != 1 ||
		indexes.MaxItems == nil || *indexes.MaxItems != 8 {
		t.Fatalf(
			"resolved role participantIndexes bounds = %v..%v, want 1..8",
			indexes.MinItems,
			indexes.MaxItems,
		)
	}

	evidence := root.Properties["status"].Properties["evidence"]
	if maxLength := evidence.Properties["uri"].MaxLength; maxLength == nil || *maxLength != 2048 {
		t.Fatalf("evidence URI maxLength = %v, want 2048", maxLength)
	}

	for _, rule := range []string{
		"!has(oldSelf.uri) || oldSelf.uri.size() == 0 || (has(self.uri) && self.uri == oldSelf.uri)",
		"!has(oldSelf.digest) || oldSelf.digest.size() == 0 || (has(self.digest) && self.digest == oldSelf.digest)",
		"!has(oldSelf.identity) || (has(self.identity) && self.identity == oldSelf.identity)",
	} {
		if !hasValidationRule(&evidence, rule) {
			t.Fatalf("evidence has no %q write-once rule", rule)
		}
	}

	if !hasValidationRule(
		root,
		"!has(self.status) || !has(self.status.evidence) || !has(self.status.evidence.identity) || (has(self.spec.planDigest) && self.status.evidence.identity.planDigest == self.spec.planDigest)",
	) {
		t.Fatal("Run does not bind evidence identity to spec.planDigest")
	}

	identity := evidence.Properties["identity"]

	participants := identity.Properties["participants"]
	if participants.MaxItems == nil || *participants.MaxItems != 8 {
		t.Fatalf("evidence participants maxItems = %v, want 8", participants.MaxItems)
	}

	checks := identity.Properties["checks"]
	if checks.MaxItems == nil || *checks.MaxItems != MaxCertificationChecks {
		t.Fatalf("evidence checks maxItems = %v, want %d", checks.MaxItems, MaxCertificationChecks)
	}

	checkSummary := checks.Items.Schema

	executionSummary := checkSummary.Properties["execution"]
	for _, property := range []string{
		"role",
		"mode",
		"configuredCoverageWindow",
		"startedAt",
		"finishedAt",
		"observedCoverageWindow",
		"invocationCount",
		"successfulInvocations",
		"timedOutInvocations",
		"failedInvocations",
		"firstInvocationAt",
		"lastInvocationAt",
		"maxStartGap",
	} {
		if _, found := executionSummary.Properties[property]; !found {
			t.Fatalf("generated evidence execution summary has no %s", property)
		}
	}

	if !hasValidationRule(
		&executionSummary,
		"self.successfulInvocations + self.timedOutInvocations + self.failedInvocations == self.invocationCount",
	) {
		t.Fatal("evidence execution summary does not reconcile invocation outcomes")
	}

	stages := identity.Properties["stages"]
	if stages.MaxItems == nil || *stages.MaxItems != MaxCertificationStages {
		t.Fatalf("evidence stages maxItems = %v, want %d", stages.MaxItems, MaxCertificationStages)
	}

	stage := stages.Items.Schema
	for _, property := range []string{
		"phaseOrder",
		"stageOrder",
		"name",
		"role",
		"test",
		"mode",
		"verdict",
		"configuredCoverageWindow",
		"observedCoverageWindow",
		"invocationCount",
		"maxStartGap",
		"deviceIntervalCoverage",
		"assertions",
		"invocationEvidence",
		"durableEvidenceDigest",
		"qualificationEligible",
		"diagnosticOnly",
	} {
		if _, found := stage.Properties[property]; !found {
			t.Fatalf("generated stage evidence has no %s", property)
		}
	}

	stageParticipants := stage.Properties["participants"]
	if stageParticipants.MaxItems == nil || *stageParticipants.MaxItems != 16 {
		t.Fatalf("stage evidence participants maxItems = %v, want 16", stageParticipants.MaxItems)
	}

	stageAssertions := stage.Properties["assertions"]
	if stageAssertions.MaxItems == nil || *stageAssertions.MaxItems != 8 {
		t.Fatalf("stage evidence assertions maxItems = %v, want 8", stageAssertions.MaxItems)
	}

	invocationEvidence := stage.Properties["invocationEvidence"]
	if invocationEvidence.MaxItems == nil || *invocationEvidence.MaxItems != 8 {
		t.Fatalf("stage invocation evidence maxItems = %v, want 8", invocationEvidence.MaxItems)
	}

	if !hasValidationRule(
		&identity,
		"(has(self.stages) && size(self.stages) > 0) != (has(self.checks) && size(self.checks) > 0)",
	) {
		t.Fatal("evidence identity does not distinguish stage and legacy check evidence")
	}

	assertionResult := stage.Properties["assertions"].Items.Schema
	if contains(assertionResult.Required, "actual") {
		t.Fatal("assertion result actual must remain optional for missing metric evidence")
	}

	if !hasValidationRule(
		assertionResult,
		"has(self.actual) || (!self.passed && has(self.message) && size(self.message.trim()) > 0)",
	) {
		t.Fatal("assertion result does not require a failed reason when actual is absent")
	}
}

func TestCertificationPolicyContract(t *testing.T) {
	t.Parallel()

	crd := readCRD(t, "signal.unbounded-cloud.io_certificationpolicies.yaml")
	if crd.Spec.Scope != apiextensionsv1.ClusterScoped {
		t.Fatalf("Policy scope = %s, want Cluster", crd.Spec.Scope)
	}

	spec := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]

	requirements := spec.Properties["requirements"]
	if requirements.MinItems == nil || *requirements.MinItems != 1 {
		t.Fatalf("requirements minItems = %v, want 1", requirements.MinItems)
	}

	if requirements.MaxItems == nil || *requirements.MaxItems != 32 {
		t.Fatalf("requirements maxItems = %v, want 32", requirements.MaxItems)
	}

	if requirements.XListType == nil || *requirements.XListType != "map" {
		t.Fatalf("requirements list type = %v, want map", requirements.XListType)
	}

	requirement := requirements.Items.Schema

	acceptedPlans := requirement.Properties["acceptedPlans"]
	if !hasValidationRule(&acceptedPlans, "self.mode != 'Listed' || (has(self.digests) && size(self.digests) > 0)") {
		t.Fatal("accepted plans do not require listed digests")
	}

	if !hasValidationRule(&spec, "self.invalidationFingerprintFields.exists(x, x == 'NodeUID')") {
		t.Fatal("Policy does not require NodeUID invalidation")
	}

	if !hasValidationRule(&spec, "self.requirements.map(x, x.requiredPassCount).sum() <= 64") {
		t.Fatal("Policy does not bound total required passes by ledger capacity")
	}

	if !hasValidationRule(
		requirement,
		"self.acceptedPlans.mode != 'Listed' || !has(self.variation) || !has(self.variation.minDistinctPlanDigests) || self.variation.minDistinctPlanDigests <= size(self.acceptedPlans.digests)",
	) {
		t.Fatal("Listed plans do not bound minDistinctPlanDigests by listed digest count")
	}

	gateMode := spec.Properties["gateProjection"].Properties["mode"]
	if !containsJSONEnum(gateMode.Enum, `"ProtectedLabel"`) {
		t.Fatalf("gate projection enum = %s, want ProtectedLabel", gateMode.Enum)
	}

	for _, contract := range []string{
		"every ProtectedLabel Policy",
		"NodeRestriction admission plugin",
		"RBAC must restrict non-node identities",
	} {
		if !strings.Contains(gateMode.Description, contract) {
			t.Fatalf("gate projection description does not contain %q", contract)
		}
	}
}

func TestNodeQualificationLabelContract(t *testing.T) {
	t.Parallel()

	const want = "node-restriction.kubernetes.io/project-signal-qualified"

	if NodeQualificationLabel != want {
		t.Fatalf("NodeQualificationLabel = %q, want %q", NodeQualificationLabel, want)
	}

	if errors := validation.IsQualifiedName(NodeQualificationLabel); len(errors) > 0 {
		t.Fatalf("NodeQualificationLabel is not a valid Kubernetes label key: %v", errors)
	}

	if NodeQualificationValue != "true" {
		t.Fatalf("NodeQualificationValue = %q, want true", NodeQualificationValue)
	}
}

func TestNodeCertificationContract(t *testing.T) {
	t.Parallel()

	crd := readCRD(t, "signal.unbounded-cloud.io_nodecertifications.yaml")
	if crd.Spec.Scope != apiextensionsv1.NamespaceScoped {
		t.Fatalf("NodeCertification scope = %s, want Namespaced", crd.Spec.Scope)
	}

	root := crd.Spec.Versions[0].Schema.OpenAPIV3Schema
	for _, contract := range []string{
		"No ledger individually owns the shared qualification label",
		"GateProjected=False",
		"NodeRestrictionUnverified",
	} {
		if !strings.Contains(root.Description, contract) {
			t.Fatalf("NodeCertification description does not contain %q", contract)
		}
	}

	spec := root.Properties["spec"]
	for _, rule := range []string{
		"self.policy.name == oldSelf.policy.name",
		"self.policy.uid == oldSelf.policy.uid",
		"self.policy.generation >= oldSelf.policy.generation",
		"self.nodeName == oldSelf.nodeName",
		"self.nodeUID == oldSelf.nodeUID",
	} {
		if !hasValidationRule(&spec, rule) {
			t.Fatalf("NodeCertification spec has no %q validation", rule)
		}
	}

	status := root.Properties["status"]

	observedGeneration := status.Properties["observedGeneration"]
	if observedGeneration.Minimum == nil || *observedGeneration.Minimum != 0 {
		t.Fatalf("observedGeneration minimum = %v, want 0", observedGeneration.Minimum)
	}

	if maxItems := status.Properties["contributingRuns"].MaxItems; maxItems == nil || *maxItems != 64 {
		t.Fatalf("contributingRuns maxItems = %v, want 64", maxItems)
	}

	if maxItems := status.Properties["planDigests"].MaxItems; maxItems == nil || *maxItems != MaxContributingRuns {
		t.Fatalf("planDigests maxItems = %v, want %d", maxItems, MaxContributingRuns)
	}

	latestNonPass, found := status.Properties["latestNonPass"]
	if !found {
		t.Fatal("NodeCertification status has no latestNonPass watermark")
	}

	for _, property := range []string{"name", "uid", "finishedAt", "classification", "evidenceDigest"} {
		if !contains(latestNonPass.Required, property) {
			t.Fatalf("latestNonPass does not require %s", property)
		}
	}

	if maxLength := latestNonPass.Properties["name"].MaxLength; maxLength == nil || *maxLength != 253 {
		t.Fatalf("latestNonPass name maxLength = %v, want 253", maxLength)
	}

	if latestNonPass.Properties["evidenceDigest"].Pattern != SHA256DigestPattern {
		t.Fatalf(
			"latestNonPass evidenceDigest pattern = %q, want %q",
			latestNonPass.Properties["evidenceDigest"].Pattern,
			SHA256DigestPattern,
		)
	}

	classification := latestNonPass.Properties["classification"]
	for _, want := range []string{`"Failed"`, `"Inconclusive"`, `"Canceled"`} {
		if !containsJSONEnum(classification.Enum, want) {
			t.Fatalf("latestNonPass classification enum = %s, missing %s", classification.Enum, want)
		}
	}

	if !hasValidationRule(
		&status,
		"!has(oldSelf.latestNonPass) || (has(self.latestNonPass) && (self.latestNonPass.finishedAt > oldSelf.latestNonPass.finishedAt || (self.latestNonPass.finishedAt == oldSelf.latestNonPass.finishedAt && (self.latestNonPass.uid > oldSelf.latestNonPass.uid || (self.latestNonPass.uid == oldSelf.latestNonPass.uid && self.latestNonPass == oldSelf.latestNonPass)))))",
	) {
		t.Fatal("latestNonPass does not have monotonic finishedAt and UID validation")
	}

	if NodeCertificationConditionGateProjected != "GateProjected" {
		t.Fatalf("gate condition = %q, want GateProjected", NodeCertificationConditionGateProjected)
	}

	if NodeCertificationReasonProtectionUnverified != "NodeRestrictionUnverified" {
		t.Fatalf(
			"unverified protection reason = %q, want NodeRestrictionUnverified",
			NodeCertificationReasonProtectionUnverified,
		)
	}

	phase := status.Properties["phase"]
	for _, want := range []string{
		`"Pending"`,
		`"Qualifying"`,
		`"Qualified"`,
		`"Expired"`,
		`"Revoked"`,
		`"NodeGone"`,
	} {
		if !containsJSONEnum(phase.Enum, want) {
			t.Fatalf("phase enum = %s, missing %s", phase.Enum, want)
		}
	}
}

func TestNodeCertificationBackwardDecode(t *testing.T) {
	t.Parallel()

	content := []byte(`{
		"apiVersion":"signal.unbounded-cloud.io/v1alpha1",
		"kind":"NodeCertification",
		"metadata":{"name":"existing","namespace":"default"},
		"spec":{
			"policy":{"name":"policy","uid":"policy-uid","generation":1},
			"nodeName":"node-1",
			"nodeUID":"node-uid"
		},
		"status":{"phase":"Qualified","observedGeneration":1}
	}`)

	ledger := &NodeCertification{}
	if err := json.Unmarshal(content, ledger); err != nil {
		t.Fatalf("decode existing NodeCertification without watermark: %v", err)
	}

	if ledger.Status.LatestNonPass != nil {
		t.Fatal("existing NodeCertification unexpectedly acquired latestNonPass")
	}
}

func TestCertificationRunBackwardDecode(t *testing.T) {
	t.Parallel()

	content := []byte(`{
		"apiVersion":"signal.unbounded-cloud.io/v1alpha1",
		"kind":"CertificationRun",
		"metadata":{"name":"existing","namespace":"default"},
		"spec":{
			"profile":{"name":"profile","uid":"profile-uid","generation":1},
			"suite":{"name":"suite","uid":"suite-uid","generation":1},
			"participants":[{"nodeName":"node-1","nodeUID":"node-uid"}],
			"scriptBundle":{"name":"scripts","uid":"scripts-uid","digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
			"checks":[{
				"script":{"key":"check.sh","digest":"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
				"runnerImage":"example.invalid/runner@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
				"backend":"Pod",
				"backendActiveDeadline":"10m"
			}]
		}
	}`)

	run := &CertificationRun{}
	if err := json.Unmarshal(content, run); err != nil {
		t.Fatalf("decode existing Run without qualification fields: %v", err)
	}

	if run.Spec.PlanDigest != "" || run.Spec.PlanProvenance != nil {
		t.Fatal("existing Run unexpectedly acquired plan fields")
	}

	if run.Spec.Checks[0].Execution != nil {
		t.Fatal("existing Run unexpectedly acquired an execution policy")
	}
}

func TestCertificationTestBackwardDecodeWithoutRoles(t *testing.T) {
	t.Parallel()

	content := []byte(`{
		"apiVersion":"signal.unbounded-cloud.io/v1alpha1",
		"kind":"CertificationTest",
		"metadata":{"name":"existing"},
		"spec":{
			"displayName":"Existing test",
			"owner":"operator",
			"version":"1",
			"metrics":[{"name":"passed","displayName":"Passed","type":"Boolean"}],
			"topology":{"minParticipants":1,"maxParticipants":1},
			"driver":{
				"type":"Container",
				"container":{"image":"example.invalid/test@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
			}
		}
	}`)

	test := &CertificationTest{}
	if err := json.Unmarshal(content, test); err != nil {
		t.Fatalf("decode existing CertificationTest without topology roles: %v", err)
	}

	if test.Spec.Topology.Roles != nil {
		t.Fatal("existing CertificationTest unexpectedly acquired topology roles")
	}
}

func TestAssertionResultBackwardDecodeAndMissingMetric(t *testing.T) {
	t.Parallel()

	existing := []byte(`{
		"name": "throughput-floor",
		"passed": true,
		"actual": {
			"type": "Decimal",
			"decimalValue": "25.0"
		}
	}`)

	var decodedExisting AssertionResult
	if err := json.Unmarshal(existing, &decodedExisting); err != nil {
		t.Fatalf("decode existing assertion result: %v", err)
	}

	if decodedExisting.Actual == nil ||
		decodedExisting.Actual.DecimalValue == nil ||
		*decodedExisting.Actual.DecimalValue != "25.0" {
		t.Fatal("existing assertion actual value was not preserved")
	}

	missing := []byte(`{
		"name": "throughput-floor",
		"passed": false,
		"message": "required throughput metric was not produced"
	}`)

	var decodedMissing AssertionResult
	if err := json.Unmarshal(missing, &decodedMissing); err != nil {
		t.Fatalf("decode missing-metric assertion result: %v", err)
	}

	if decodedMissing.Actual != nil || decodedMissing.Passed || decodedMissing.Message == "" {
		t.Fatal("missing-metric assertion result did not preserve failed evidence without actual")
	}

	encodedMissing, err := json.Marshal(decodedMissing)
	if err != nil {
		t.Fatalf("encode missing-metric assertion result: %v", err)
	}

	if strings.Contains(string(encodedMissing), `"actual"`) {
		t.Fatalf("missing-metric assertion result encoded an invented actual value: %s", encodedMissing)
	}
}

func TestCertificationSuiteBackwardDecode(t *testing.T) {
	t.Parallel()

	content := []byte(`{
		"apiVersion":"signal.unbounded-cloud.io/v1alpha1",
		"kind":"CertificationSuite",
		"metadata":{"name":"existing"},
		"spec":{
			"scriptBundle":{"name":"scripts","digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
			"checks":[{
				"scriptKey":"check.sh",
				"backend":"Pod",
				"backendActiveDeadline":"10m",
				"minDuration":"5m"
			}]
		}
	}`)

	suite := &CertificationSuite{}
	if err := json.Unmarshal(content, suite); err != nil {
		t.Fatalf("decode existing Suite without execution policy: %v", err)
	}

	if suite.Spec.Checks[0].Execution != nil {
		t.Fatal("existing Suite unexpectedly acquired an execution policy")
	}

	if EffectiveCheckExecutionMode(suite.Spec.Checks[0].Execution) != CheckExecutionModeRunOnce {
		t.Fatal("existing Suite does not retain legacy RunOnce behavior")
	}

	if SupportsSustainedExecution(suite.Spec.Checks[0].Execution) {
		t.Fatal("legacy minDuration unexpectedly qualifies as sustained execution")
	}
}

func readCRD(t *testing.T, name string) *apiextensionsv1.CustomResourceDefinition {
	t.Helper()

	content, err := os.ReadFile(filepath.Join("..", "..", "..", "deploy", "signal", "crd", name))
	if err != nil {
		t.Fatalf("read generated CRD: %v", err)
	}

	jsonContent, err := yaml.ToJSON(content)
	if err != nil {
		t.Fatalf("convert generated CRD to JSON: %v", err)
	}

	crd := &apiextensionsv1.CustomResourceDefinition{}
	if err := json.Unmarshal(jsonContent, crd); err != nil {
		t.Fatalf("decode generated CRD: %v", err)
	}

	return crd
}

func validateCRDCEL(
	t *testing.T,
	crd *apiextensionsv1.CustomResourceDefinition,
	object map[string]any,
) field.ErrorList {
	t.Helper()

	return validateCRDCELUpdate(t, crd, object, nil)
}

func validateCRDCELUpdate(
	t *testing.T,
	crd *apiextensionsv1.CustomResourceDefinition,
	object map[string]any,
	oldObject map[string]any,
) field.ErrorList {
	t.Helper()

	versioned := crd.Spec.Versions[0].Schema.OpenAPIV3Schema

	internal := &apiextensions.JSONSchemaProps{}
	if err := apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(
		versioned,
		internal,
		nil,
	); err != nil {
		t.Fatalf("convert CRD schema: %v", err)
	}

	structural, err := schema.NewStructural(internal)
	if err != nil {
		t.Fatalf("build structural schema: %v", err)
	}

	const costBudget = 10_000_000

	validator := schemacel.NewValidator(structural, true, costBudget)
	if validator == nil {
		t.Fatal("generated CRD has no CEL validator")
	}

	errors, _ := validator.Validate(
		context.Background(),
		field.NewPath("object"),
		structural,
		object,
		oldObject,
		costBudget,
	)

	return errors
}

func unstructuredObject(t *testing.T, object any) map[string]any {
	t.Helper()

	unstructured, err := runtime.DefaultUnstructuredConverter.ToUnstructured(object)
	if err != nil {
		t.Fatalf("convert unstructured object: %v", err)
	}

	return unstructured
}

func deepCopyUnstructured(t *testing.T, object map[string]any) map[string]any {
	t.Helper()

	return runtime.DeepCopyJSON(object)
}

func hasValidationRule(schema *apiextensionsv1.JSONSchemaProps, want string) bool {
	for _, validation := range schema.XValidations {
		if validation.Rule == want {
			return true
		}
	}

	return false
}

func contains[T comparable](values []T, want T) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}

	return false
}

func containsJSONEnum(values []apiextensionsv1.JSON, want string) bool {
	for _, value := range values {
		if string(value.Raw) == want {
			return true
		}
	}

	return false
}
