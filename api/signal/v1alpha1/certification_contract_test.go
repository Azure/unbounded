// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/yaml"
)

func TestCertificationKindsRegistered(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		t.Fatalf("add Signal types to scheme: %v", err)
	}

	for _, object := range []runtime.Object{
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

	for _, property := range []string{"minDuration", "continuity"} {
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

			if tt.schemaPath[0] == "spec" {
				if schema.MinItems == nil || *schema.MinItems != MinCertificationChecks {
					t.Fatalf("minItems = %v, want %d", schema.MinItems, MinCertificationChecks)
				}
			}

			if schema.XListType == nil || *schema.XListType != "atomic" {
				t.Fatalf("x-kubernetes-list-type = %v, want atomic", schema.XListType)
			}
		})
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
		"self.profile == oldSelf.profile":           false,
		"self.suite == oldSelf.suite":               false,
		"self.scriptBundle == oldSelf.scriptBundle": false,
		"self.checks == oldSelf.checks":             false,
		"has(self.planDigest) == has(oldSelf.planDigest) && (!has(self.planDigest) || self.planDigest == oldSelf.planDigest)":                     false,
		"has(self.planProvenance) == has(oldSelf.planProvenance) && (!has(self.planProvenance) || self.planProvenance == oldSelf.planProvenance)": false,
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

	evidence := root.Properties["status"].Properties["evidence"]
	if maxLength := evidence.Properties["uri"].MaxLength; maxLength == nil || *maxLength != 2048 {
		t.Fatalf("evidence URI maxLength = %v, want 2048", maxLength)
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

	gateMode := spec.Properties["gateProjection"].Properties["mode"]
	if !containsJSONEnum(gateMode.Enum, `"ProtectedLabel"`) {
		t.Fatalf("gate projection enum = %s, want ProtectedLabel", gateMode.Enum)
	}
}

func TestNodeCertificationContract(t *testing.T) {
	t.Parallel()

	crd := readCRD(t, "signal.unbounded-cloud.io_nodecertifications.yaml")
	if crd.Spec.Scope != apiextensionsv1.NamespaceScoped {
		t.Fatalf("NodeCertification scope = %s, want Namespaced", crd.Spec.Scope)
	}

	root := crd.Spec.Versions[0].Schema.OpenAPIV3Schema

	spec := root.Properties["spec"]
	for _, rule := range []string{
		"self.policy == oldSelf.policy",
		"self.nodeName == oldSelf.nodeName",
		"self.nodeUID == oldSelf.nodeUID",
	} {
		if !hasValidationRule(&spec, rule) {
			t.Fatalf("NodeCertification spec has no %q validation", rule)
		}
	}

	status := root.Properties["status"]
	if maxItems := status.Properties["contributingRuns"].MaxItems; maxItems == nil || *maxItems != 64 {
		t.Fatalf("contributingRuns maxItems = %v, want 64", maxItems)
	}

	if maxItems := status.Properties["planDigests"].MaxItems; maxItems == nil || *maxItems != 32 {
		t.Fatalf("planDigests maxItems = %v, want 32", maxItems)
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
