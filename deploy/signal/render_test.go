// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signal

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"github.com/Azure/unbounded/hack/cmd/render-manifests/render"
)

const (
	testControllerImage = "example.invalid/controller@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testAdapterImage    = "example.invalid/adapter@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	testRunnerImage     = "example.invalid/runner@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	testResolverImage   = "example.invalid/resolver@sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	testDigest          = "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	testScriptDigest    = "sha256:c6386f7989280ce844b5e997377839434684f4c6b52dbd8f3d7d15def1fc72a3"
	qualificationGuard  = "node-authorizer-node-restriction-rbac-verified"
)

func TestRenderedExecutionProvenanceIsPinnedAndDeterministic(t *testing.T) {
	t.Parallel()

	firstOutput := renderSignal(t, signalRenderData())
	firstWorkflow := readRenderedFile(t, firstOutput, "04-adapter-workflowtemplate.yaml")
	sum := sha256.Sum256(firstWorkflow)
	workflowDigest := fmt.Sprintf("sha256:%x", sum)

	data := signalRenderData()
	data["WorkflowTemplateDigest"] = workflowDigest
	secondOutput := renderSignal(t, data)
	secondWorkflow := readRenderedFile(t, secondOutput, "04-adapter-workflowtemplate.yaml")

	if !bytes.Equal(firstWorkflow, secondWorkflow) {
		t.Fatal("WorkflowTemplate bytes changed when only its recorded digest changed")
	}

	changedData := signalRenderData()
	changedData["AdapterImage"] = "example.invalid/adapter@sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"

	changedWorkflow := readRenderedFile(t, renderSignal(t, changedData), "04-adapter-workflowtemplate.yaml")
	if sha256.Sum256(changedWorkflow) == sum {
		t.Fatal("WorkflowTemplate digest did not change with adapter image semantics")
	}

	var workflow struct {
		Spec struct {
			Templates []struct {
				Name      string `json:"name"`
				Container struct {
					Image string `json:"image"`
				} `json:"container"`
			} `json:"templates"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal(secondWorkflow, &workflow); err != nil {
		t.Fatalf("decode WorkflowTemplate: %v", err)
	}

	if len(workflow.Spec.Templates) != 1 ||
		workflow.Spec.Templates[0].Name != "certification" ||
		workflow.Spec.Templates[0].Container.Image != testAdapterImage {
		t.Fatalf("WorkflowTemplate adapter identity = %#v", workflow.Spec.Templates)
	}

	for _, parameter := range []string{"check-index", "phase-index", "stage-index"} {
		if !bytes.Contains(secondWorkflow, []byte("- name: "+parameter)) {
			t.Fatalf("WorkflowTemplate does not declare %s", parameter)
		}
	}

	var deployment appsv1.Deployment
	if err := yaml.Unmarshal(readRenderedFile(t, secondOutput, "06-deployment.yaml"), &deployment); err != nil {
		t.Fatalf("decode Deployment: %v", err)
	}

	if len(deployment.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("controller containers = %d, want 1", len(deployment.Spec.Template.Spec.Containers))
	}

	controller := deployment.Spec.Template.Spec.Containers[0]
	if controller.Image != testControllerImage {
		t.Fatalf("controller image = %q, want %q", controller.Image, testControllerImage)
	}

	for _, argument := range []string{
		"--script-namespace=script-ns",
		"--runner-image=" + testRunnerImage,
		"--controller-image=" + testControllerImage,
		"--adapter-image=" + testAdapterImage,
		"--workflow-template-digest=" + workflowDigest,
		"--resolver-image=" + testResolverImage,
		"--qualification-gate-protected=false",
	} {
		if !slices.Contains(controller.Args, argument) {
			t.Fatalf("controller args do not contain %q: %v", argument, controller.Args)
		}
	}
}

func TestRenderedGPUInventoryCatalogPinsSource(t *testing.T) {
	t.Parallel()

	output := renderSignal(t, signalRenderData())
	documents := splitYAMLDocuments(string(readRenderedFile(t, output, "05-gpu-inventory-tests.yaml")))

	if len(documents) != 3 {
		t.Fatalf("GPU inventory documents = %d, want 3", len(documents))
	}

	var scripts corev1.ConfigMap
	if err := yaml.Unmarshal([]byte(documents[0]), &scripts); err != nil {
		t.Fatalf("decode GPU inventory ConfigMap: %v", err)
	}

	script := scripts.Data["gpu-inventory.sh"]
	scriptSum := sha256.Sum256([]byte(script))
	scriptDigest := fmt.Sprintf("sha256:%x", scriptSum)

	if scriptDigest != testScriptDigest {
		t.Fatalf("GPU inventory script digest = %s, test fixture expects %s", scriptDigest, testScriptDigest)
	}

	for index, wantRole := range []string{"qualification", "compatibility-diagnostic"} {
		var test unstructured.Unstructured
		if err := yaml.Unmarshal([]byte(documents[index+1]), &test.Object); err != nil {
			t.Fatalf("decode CertificationTest %d: %v", index, err)
		}

		if test.GetKind() != "CertificationTest" ||
			test.GetLabels()["signal.unbounded-cloud.io/catalog-role"] != wantRole {
			t.Fatalf("CertificationTest %d identity = %s/%v", index, test.GetKind(), test.GetLabels())
		}

		gotDigest, found, err := unstructured.NestedString(
			test.Object,
			"spec",
			"driver",
			"script",
			"source",
			"configMap",
			"digest",
		)
		if err != nil || !found || gotDigest != scriptDigest {
			t.Fatalf(
				"CertificationTest %d script digest = %q, found %t, error %v, want %s",
				index,
				gotDigest,
				found,
				err,
				scriptDigest,
			)
		}

		gotGPU, found, err := unstructured.NestedString(
			test.Object,
			"spec",
			"driver",
			"script",
			"resources",
			"requests",
			"nvidia.com/gpu",
		)
		if err != nil || !found || gotGPU != "8" {
			t.Fatalf(
				"CertificationTest %d GPU request = %q, found %t, error %v, want 8",
				index,
				gotGPU,
				found,
				err,
			)
		}
	}
}

func TestRenderedVLLMCanaryUsesImmutableBoundedContract(t *testing.T) {
	t.Parallel()

	data := signalRenderData()
	data["VLLMCanaryEnabled"] = "true"
	data["VLLMCanaryImage"] = "example.invalid/vllm-canary@sha256:" + strings.Repeat("f", 64)
	data["VLLMCanaryCPU"] = "4"
	data["VLLMCanaryMemory"] = "16Gi"
	data["VLLMModelCachePVC"] = "approved-model-cache"
	data["VLLMWorkflowTemplateDigest"] = testDigest

	firstOutput := renderSignal(t, data)
	firstTemplate := readRenderedFile(t, firstOutput, "07-vllm-canary-workflowtemplate.yaml")

	var workflow unstructured.Unstructured
	if err := yaml.Unmarshal(firstTemplate, &workflow.Object); err != nil {
		t.Fatalf("decode vLLM WorkflowTemplate: %v", err)
	}

	spec, found, err := unstructured.NestedMap(workflow.Object, "spec")
	if err != nil || !found {
		t.Fatalf("read vLLM WorkflowTemplate spec: found %t, error %v", found, err)
	}

	specJSON, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("encode vLLM WorkflowTemplate spec: %v", err)
	}

	sum := sha256.Sum256(specJSON)
	workflowDigest := fmt.Sprintf("sha256:%x", sum)
	data["VLLMWorkflowTemplateDigest"] = workflowDigest

	secondOutput := renderSignal(t, data)
	if !bytes.Equal(
		firstTemplate,
		readRenderedFile(t, secondOutput, "07-vllm-canary-workflowtemplate.yaml"),
	) {
		t.Fatal("vLLM WorkflowTemplate changed when only its recorded digest changed")
	}

	templateText := string(firstTemplate)
	for _, value := range []string{
		"primary-participant-node-name",
		"resolved-inputs-json",
		"result-configmap",
		"nvidia.com/gpu: \"8\"",
		"readOnly: true",
	} {
		if !strings.Contains(templateText, value) {
			t.Fatalf("vLLM WorkflowTemplate does not contain %q", value)
		}
	}

	var test unstructured.Unstructured
	if err := yaml.Unmarshal(
		readRenderedFile(t, secondOutput, "08-vllm-canary-test.yaml"),
		&test.Object,
	); err != nil {
		t.Fatalf("decode vLLM CertificationTest: %v", err)
	}

	gotDigest, found, err := unstructured.NestedString(
		test.Object,
		"spec",
		"driver",
		"workflowTemplate",
		"digest",
	)
	if err != nil || !found || gotDigest != workflowDigest {
		t.Fatalf(
			"vLLM test template digest = %q, found %t, error %v, want %s",
			gotDigest,
			found,
			err,
			workflowDigest,
		)
	}

	testText := string(readRenderedFile(t, secondOutput, "08-vllm-canary-test.yaml"))
	for _, metric := range []string{
		"serviceReady",
		"modelIdentity",
		"modelRevision",
		"modelWeightsDigest",
		"requestSucceeded",
		"promptTokens",
		"completionTokens",
		"serverErrorCount",
		"requestErrorCount",
		"containerRestartCount",
		"workflowPodRestartCount",
		"tensorParallelSize",
		"observedGpuCount",
		"activeGpuCount",
		"cleanupSucceeded",
		"endToEndLatency",
		"timeToFirstToken",
		"outputTokensPerSecond",
		"gpuMemoryUsed",
	} {
		if !strings.Contains(testText, "name: "+metric) {
			t.Fatalf("vLLM CertificationTest does not declare metric %s", metric)
		}
	}

	for _, parameter := range []string{
		"expectedModelIdentity",
		"expectedModelRevision",
		"expectedModelWeightsDigest",
		"promptFixtureDigest",
		"requestTimeout",
	} {
		if !strings.Contains(testText, "name: "+parameter) {
			t.Fatalf("vLLM CertificationTest does not declare parameter %s", parameter)
		}
	}

	if strings.Contains(testText, "assertions:") ||
		strings.Contains(testText, "minimum:") ||
		strings.Contains(testText, "maximum:") {
		t.Fatal("vLLM CertificationTest invents workload performance thresholds")
	}
}

func TestVLLMCanaryFailsClosedWithoutImmutableInputs(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name  string
		key   string
		value string
		error string
	}{
		{
			name:  "mutable image",
			key:   "VLLMCanaryImage",
			value: "example.invalid/vllm-canary:latest",
			error: "VLLMCanaryImage must be an immutable sha256 digest reference",
		},
		{
			name:  "missing CPU",
			key:   "VLLMCanaryCPU",
			value: "",
			error: "VLLMCanaryCPU is required",
		},
		{
			name:  "missing memory",
			key:   "VLLMCanaryMemory",
			value: "",
			error: "VLLMCanaryMemory is required",
		},
		{
			name:  "missing model cache",
			key:   "VLLMModelCachePVC",
			value: "",
			error: "VLLMModelCachePVC is required",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			data := signalRenderData()
			data["VLLMCanaryEnabled"] = "true"
			data["VLLMCanaryImage"] = "example.invalid/vllm-canary@sha256:" + strings.Repeat("f", 64)
			data["VLLMCanaryCPU"] = "4"
			data["VLLMCanaryMemory"] = "16Gi"
			data["VLLMModelCachePVC"] = "approved-model-cache"
			data["VLLMWorkflowTemplateDigest"] = testDigest
			data[tt.key] = tt.value

			err := render.Render(".", t.TempDir(), data)
			if err == nil || !strings.Contains(err.Error(), tt.error) {
				t.Fatalf("render error = %v, want %q", err, tt.error)
			}
		})
	}
}

func TestRenderedAcceptanceSuitesSeparateQualificationDiagnosticsAndVLLM(t *testing.T) {
	t.Parallel()

	data := signalRenderData()
	data["AcceptanceSuitesEnabled"] = "true"
	data["GPUInventoryTestUID"] = "11111111-1111-1111-1111-111111111111"
	data["GPUInventoryTestGeneration"] = "1"
	data["GPUInventoryTestContentDigest"] = testDigest
	data["GPUCompatTestUID"] = "22222222-2222-2222-2222-222222222222"
	data["GPUCompatTestGeneration"] = "2"
	data["GPUCompatTestContentDigest"] = testDigest
	data["VLLMCanaryEnabled"] = "true"
	data["VLLMCanaryImage"] = "example.invalid/vllm-canary@sha256:" + strings.Repeat("f", 64)
	data["VLLMCanaryCPU"] = "4"
	data["VLLMCanaryMemory"] = "16Gi"
	data["VLLMModelCachePVC"] = "approved-model-cache"
	data["VLLMTestUID"] = "33333333-3333-3333-3333-333333333333"
	data["VLLMTestGeneration"] = "3"
	data["VLLMTestContentDigest"] = testDigest
	data["VLLMModelIdentity"] = "approved-model@sha256:" + strings.Repeat("9", 64)
	data["VLLMModelRevision"] = strings.Repeat("7", 40)
	data["VLLMModelWeightsDigest"] = "sha256:" + strings.Repeat("6", 64)
	data["VLLMPromptFixtureDigest"] = "sha256:" + strings.Repeat("8", 64)
	data["VLLMRequestTimeout"] = "2m"
	data["VLLMInvocationTimeout"] = "30m"

	output := renderSignal(t, data)
	documents := splitYAMLDocuments(string(readRenderedFile(t, output, "09-acceptance-suites.yaml")))

	if len(documents) != 3 {
		t.Fatalf("acceptance Suite documents = %d, want 3", len(documents))
	}

	var baseline unstructured.Unstructured
	if err := yaml.Unmarshal([]byte(documents[0]), &baseline.Object); err != nil {
		t.Fatalf("decode baseline Suite: %v", err)
	}

	baselinePhases, found, err := unstructured.NestedSlice(baseline.Object, "spec", "phases")
	if err != nil || !found || len(baselinePhases) != 1 {
		t.Fatalf("baseline phases = %d, found %t, error %v, want 1", len(baselinePhases), found, err)
	}

	if strings.Count(documents[0], "name: project-signal-gpu-inventory-v1") != 1 ||
		strings.Contains(documents[0], "project-signal-gpu-inventory-compat-v1") {
		t.Fatal("baseline Suite must contain exactly one canonical qualification Test")
	}

	var compatibility unstructured.Unstructured
	if err := yaml.Unmarshal([]byte(documents[1]), &compatibility.Object); err != nil {
		t.Fatalf("decode compatibility Suite: %v", err)
	}

	compatibilityPhases, found, err := unstructured.NestedSlice(
		compatibility.Object,
		"spec",
		"phases",
	)
	if err != nil || !found || len(compatibilityPhases) != 15 {
		t.Fatalf(
			"compatibility phases = %d, found %t, error %v, want 15 ordered diagnostics",
			len(compatibilityPhases),
			found,
			err,
		)
	}

	if !strings.Contains(documents[1], "project-signal-gpu-inventory-compat-v1") ||
		strings.Contains(documents[1], "name: project-signal-gpu-inventory-v1") {
		t.Fatal("compatibility Suite must reference only the diagnostic compatibility Test")
	}

	vllmText := documents[2]
	for _, metric := range []string{
		"serviceReady",
		"modelIdentity",
		"modelRevision",
		"modelWeightsDigest",
		"requestSucceeded",
		"promptTokens",
		"completionTokens",
		"serverErrorCount",
		"requestErrorCount",
		"containerRestartCount",
		"workflowPodRestartCount",
		"tensorParallelSize",
		"observedGpuCount",
		"activeGpuCount",
		"cleanupSucceeded",
	} {
		if !strings.Contains(vllmText, "metric: "+metric) {
			t.Fatalf("vLLM Suite does not assert %s", metric)
		}
	}

	for _, measurementOnly := range []string{
		"endToEndLatency",
		"timeToFirstToken",
		"outputTokensPerSecond",
		"gpuMemoryUsed",
	} {
		if strings.Contains(vllmText, "metric: "+measurementOnly) {
			t.Fatalf("vLLM Suite asserts measurement-only metric %s", measurementOnly)
		}
	}
}

func TestRenderedVLLMAcceptanceFailsClosedWithoutExactModelProvenance(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name  string
		key   string
		value string
		error string
	}{
		{
			name:  "missing model revision",
			key:   "VLLMModelRevision",
			value: "",
			error: "VLLMModelRevision must be an exact 40-64 character hexadecimal revision",
		},
		{
			name:  "mutable model weights",
			key:   "VLLMModelWeightsDigest",
			value: "weights-latest",
			error: "VLLMModelWeightsDigest must be an algorithm-qualified sha256 digest",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			data := signalRenderData()
			data["AcceptanceSuitesEnabled"] = "true"
			data["GPUInventoryTestUID"] = "11111111-1111-1111-1111-111111111111"
			data["GPUInventoryTestGeneration"] = "1"
			data["GPUInventoryTestContentDigest"] = testDigest
			data["GPUCompatTestUID"] = "22222222-2222-2222-2222-222222222222"
			data["GPUCompatTestGeneration"] = "2"
			data["GPUCompatTestContentDigest"] = testDigest
			data["VLLMCanaryEnabled"] = "true"
			data["VLLMCanaryImage"] = "example.invalid/vllm-canary@sha256:" + strings.Repeat("f", 64)
			data["VLLMCanaryCPU"] = "4"
			data["VLLMCanaryMemory"] = "16Gi"
			data["VLLMModelCachePVC"] = "approved-model-cache"
			data["VLLMTestUID"] = "33333333-3333-3333-3333-333333333333"
			data["VLLMTestGeneration"] = "3"
			data["VLLMTestContentDigest"] = testDigest
			data["VLLMModelIdentity"] = "approved-model@sha256:" + strings.Repeat("9", 64)
			data["VLLMModelRevision"] = strings.Repeat("7", 40)
			data["VLLMModelWeightsDigest"] = "sha256:" + strings.Repeat("6", 64)
			data["VLLMPromptFixtureDigest"] = "sha256:" + strings.Repeat("8", 64)
			data["VLLMRequestTimeout"] = "2m"
			data["VLLMInvocationTimeout"] = "30m"
			data[tt.key] = tt.value

			err := render.Render(".", t.TempDir(), data)
			if err == nil || !strings.Contains(err.Error(), tt.error) {
				t.Fatalf("render error = %v, want %q", err, tt.error)
			}
		})
	}
}

func TestRenderRejectsMutableExecutionProvenance(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name  string
		key   string
		value string
		error string
	}{
		{
			name:  "controller image",
			key:   "ControllerImage",
			value: "example.invalid/controller:latest",
			error: "ControllerImage must be an immutable sha256 digest reference",
		},
		{
			name:  "adapter image",
			key:   "AdapterImage",
			value: "example.invalid/adapter:latest",
			error: "AdapterImage must be an immutable sha256 digest reference",
		},
		{
			name:  "runner image",
			key:   "RunnerImage",
			value: "example.invalid/runner:latest",
			error: "RunnerImage must be an immutable sha256 digest reference",
		},
		{
			name:  "resolver image",
			key:   "ResolverImages",
			value: "example.invalid/resolver:latest",
			error: "ResolverImages entries must be immutable sha256 digest references",
		},
		{
			name:  "GPU inventory script digest",
			key:   "GPUInventoryScriptDigest",
			value: "sha256:not-a-digest",
			error: "GPUInventoryScriptDigest must be an algorithm-qualified sha256 digest",
		},
		{
			name:  "workflow digest",
			key:   "WorkflowTemplateDigest",
			value: "sha256:not-a-digest",
			error: "WorkflowTemplateDigest must be an algorithm-qualified sha256 digest",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			data := signalRenderData()
			data[test.key] = test.value

			err := render.Render(".", t.TempDir(), data)
			if err == nil || !strings.Contains(err.Error(), test.error) {
				t.Fatalf("render error = %v, want %q", err, test.error)
			}
		})
	}
}

func TestQualificationGateRequiresExplicitGuard(t *testing.T) {
	t.Parallel()

	data := signalRenderData()
	data["QualificationGateProtected"] = "true"

	if err := render.Render(".", t.TempDir(), data); err == nil ||
		!strings.Contains(err.Error(), "explicit Node authorizer") {
		t.Fatalf("unguarded protected gate error = %v", err)
	}

	data["QualificationGateGuard"] = qualificationGuard
	output := renderSignal(t, data)

	var deployment appsv1.Deployment
	if err := yaml.Unmarshal(readRenderedFile(t, output, "06-deployment.yaml"), &deployment); err != nil {
		t.Fatalf("decode guarded Deployment: %v", err)
	}

	if !slices.Contains(
		deployment.Spec.Template.Spec.Containers[0].Args,
		"--qualification-gate-protected=true",
	) {
		t.Fatalf("guarded Deployment does not enable qualification projection")
	}

	data = signalRenderData()
	data["QualificationGateProtected"] = "enabled"

	if err := render.Render(".", t.TempDir(), data); err == nil ||
		!strings.Contains(err.Error(), "must be true or false") {
		t.Fatalf("invalid qualification gate error = %v", err)
	}
}

func TestRenderedRBACCoversQualificationAndEvidence(t *testing.T) {
	t.Parallel()

	output := renderSignal(t, signalRenderData())
	controllerDocuments := splitYAMLDocuments(string(readRenderedFile(t, output, "02-rbac.yaml")))

	assertRule(
		t,
		controllerDocuments,
		"ClusterRole",
		"",
		"project-signal-controller",
		"",
		"nodes",
		[]string{"get", "list", "watch", "update", "patch"},
	)
	assertRule(
		t,
		controllerDocuments,
		"ClusterRole",
		"",
		"project-signal-controller",
		"signal.unbounded-cloud.io",
		"certificationtests",
		[]string{"get", "list", "watch"},
	)
	assertRule(
		t,
		controllerDocuments,
		"ClusterRole",
		"",
		"project-signal-controller",
		"signal.unbounded-cloud.io",
		"certificationpolicies",
		[]string{"get", "list", "watch", "update", "patch"},
	)
	assertRule(
		t,
		controllerDocuments,
		"Role",
		"signal-ns",
		"project-signal-controller",
		"signal.unbounded-cloud.io",
		"certificationruns",
		[]string{"get", "list", "watch", "create", "update", "patch", "delete"},
	)
	assertRule(
		t,
		controllerDocuments,
		"Role",
		"signal-ns",
		"project-signal-controller",
		"signal.unbounded-cloud.io",
		"nodecertifications",
		[]string{"get", "list", "watch", "create", "update", "patch", "delete"},
	)

	adapterDocuments := splitYAMLDocuments(string(readRenderedFile(
		t,
		output,
		"03-adapter-serviceaccount-rbac.yaml",
	)))
	assertRule(
		t,
		adapterDocuments,
		"Role",
		"signal-ns",
		"project-signal-adapter",
		"argoproj.io",
		"workflowtaskresults",
		[]string{"create", "patch"},
	)
	assertRule(
		t,
		adapterDocuments,
		"Role",
		"signal-ns",
		"project-signal-adapter",
		"argoproj.io",
		"workflows",
		[]string{"get", "create", "delete"},
	)
	assertRule(
		t,
		adapterDocuments,
		"Role",
		"signal-ns",
		"project-signal-adapter",
		"argoproj.io",
		"workflowtemplates",
		[]string{"get"},
	)
	assertRule(
		t,
		adapterDocuments,
		"Role",
		"signal-ns",
		"project-signal-adapter",
		"",
		"configmaps",
		[]string{"get", "create", "delete"},
	)
	assertRule(
		t,
		adapterDocuments,
		"ClusterRole",
		"",
		"project-signal-adapter-node-reader",
		"",
		"nodes",
		[]string{"get"},
	)
}

func TestGeneratedCRDSetContainsAllCertificationResources(t *testing.T) {
	t.Parallel()

	want := []string{
		"signal.unbounded-cloud.io_certificationpolicies.yaml",
		"signal.unbounded-cloud.io_certificationprofiles.yaml",
		"signal.unbounded-cloud.io_certificationruns.yaml",
		"signal.unbounded-cloud.io_certificationsuites.yaml",
		"signal.unbounded-cloud.io_certificationtests.yaml",
		"signal.unbounded-cloud.io_nodecertifications.yaml",
	}

	entries, err := os.ReadDir("crd")
	if err != nil {
		t.Fatalf("read CRD directory: %v", err)
	}

	var got []string

	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".yaml") {
			got = append(got, entry.Name())
		}
	}

	slices.Sort(got)

	if !slices.Equal(got, want) {
		t.Fatalf("generated CRDs = %v, want %v", got, want)
	}

	for _, name := range got {
		content, err := os.ReadFile(filepath.Join("crd", name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}

		var crd apiextensionsv1.CustomResourceDefinition
		if err := yaml.Unmarshal(content, &crd); err != nil {
			t.Fatalf("decode %s: %v", name, err)
		}

		if crd.Spec.PreserveUnknownFields ||
			len(crd.Spec.Versions) != 1 ||
			crd.Spec.Versions[0].Schema == nil ||
			crd.Spec.Versions[0].Schema.OpenAPIV3Schema == nil {
			t.Fatalf("%s is not a structural v1 CRD: %#v", name, crd.Spec)
		}

		switch name {
		case "signal.unbounded-cloud.io_certificationruns.yaml",
			"signal.unbounded-cloud.io_certificationsuites.yaml":
			assertContainsAll(
				t,
				string(content),
				name,
				"invocationTimeout",
				"coverageWindow",
				"RunOnce",
				"repeated-execution",
				"Continuous and Periodic",
			)
		case "signal.unbounded-cloud.io_certificationtests.yaml":
			assertContainsAll(
				t,
				string(content),
				name,
				"Approved",
				"Diagnostic",
				"WorkflowTemplate",
				"RunnerImage is the approved digest-pinned runtime",
				"script resource requests must not exceed 16 entries",
			)
		case "signal.unbounded-cloud.io_nodecertifications.yaml":
			assertContainsAll(
				t,
				string(content),
				name,
				"latestNonPass",
				"latestNonPass must advance by finishedAt and UID",
			)
		}
	}
}

func signalRenderData() map[string]string {
	return map[string]string{
		"Namespace":                  "signal-ns",
		"ScriptNamespace":            "script-ns",
		"ControllerImage":            testControllerImage,
		"AdapterImage":               testAdapterImage,
		"RunnerImage":                testRunnerImage,
		"ResolverImages":             testResolverImage,
		"GPUInventoryScriptDigest":   testScriptDigest,
		"VLLMCanaryEnabled":          "false",
		"AcceptanceSuitesEnabled":    "false",
		"VLLMWorkflowTemplateDigest": testDigest,
		"WorkflowTemplateDigest":     testDigest,
		"QualificationGateProtected": "false",
	}
}

func renderSignal(t *testing.T, data map[string]string) string {
	t.Helper()

	output := t.TempDir()
	if err := render.Render(".", output, data); err != nil {
		t.Fatalf("render Signal manifests: %v", err)
	}

	return output
}

func readRenderedFile(t *testing.T, output, name string) []byte {
	t.Helper()

	content, err := os.ReadFile(filepath.Join(output, name))
	if err != nil {
		t.Fatalf("read rendered %s: %v", name, err)
	}

	return content
}

func splitYAMLDocuments(content string) []string {
	var documents []string

	for _, document := range strings.Split(content, "\n---") {
		if strings.TrimSpace(document) != "" {
			documents = append(documents, document)
		}
	}

	return documents
}

func assertRule(
	t *testing.T,
	documents []string,
	kind string,
	namespace string,
	name string,
	group string,
	resource string,
	verbs []string,
) {
	t.Helper()

	for _, document := range documents {
		var object struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name      string `json:"name"`
				Namespace string `json:"namespace"`
			} `json:"metadata"`
			Rules []rbacv1.PolicyRule `json:"rules"`
		}
		if err := yaml.Unmarshal([]byte(document), &object); err != nil {
			t.Fatalf("decode RBAC document: %v", err)
		}

		if object.Kind != kind || object.Metadata.Name != name || object.Metadata.Namespace != namespace {
			continue
		}

		for _, rule := range object.Rules {
			if slices.Equal(rule.APIGroups, []string{group}) &&
				slices.Equal(rule.Resources, []string{resource}) &&
				slices.Equal(rule.Verbs, verbs) {
				return
			}
		}

		t.Fatalf("%s/%s has no %s/%s rule with verbs %v: %#v", kind, name, group, resource, verbs, object.Rules)
	}

	t.Fatalf("%s/%s in namespace %q not found", kind, name, namespace)
}

func assertContainsAll(t *testing.T, content, name string, values ...string) {
	t.Helper()

	for _, value := range values {
		if !strings.Contains(content, value) {
			t.Fatalf("%s does not contain %q", name, value)
		}
	}
}
