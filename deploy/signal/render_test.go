// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signal

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
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
	testPrometheusImage = "example.invalid/prometheus@sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	testDigest          = "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	testScriptDigest    = "sha256:e29ac9e1988d6c6c2837f2ab3e480c54854dd315f030c4aef994019659a7b635"
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
					Image string   `json:"image"`
					Args  []string `json:"args"`
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

	adapterArgs := workflow.Spec.Templates[0].Container.Args

	serviceAccountFlag := slices.Index(adapterArgs, "--workflow-service-account")
	if serviceAccountFlag < 0 || serviceAccountFlag+1 >= len(adapterArgs) ||
		adapterArgs[serviceAccountFlag+1] != "project-signal-adapter" {
		t.Fatalf("WorkflowTemplate adapter args do not select the default child Workflow ServiceAccount: %v", adapterArgs)
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
		"--max-concurrent-runs=1",
	} {
		if !slices.Contains(controller.Args, argument) {
			t.Fatalf("controller args do not contain %q: %v", argument, controller.Args)
		}
	}
}

func TestRenderedPrometheusScrapesSignalController(t *testing.T) {
	t.Parallel()

	output := renderSignal(t, signalRenderData())

	documents := splitYAMLDocuments(string(readRenderedFile(t, output, "13-prometheus.yaml")))
	if len(documents) != 7 {
		t.Fatalf("Prometheus documents = %d, want 7", len(documents))
	}

	var serviceAccount corev1.ServiceAccount
	if err := yaml.Unmarshal([]byte(documents[0]), &serviceAccount); err != nil {
		t.Fatalf("decode Prometheus ServiceAccount: %v", err)
	}

	if serviceAccount.Name != "project-signal-prometheus" ||
		serviceAccount.AutomountServiceAccountToken == nil ||
		!*serviceAccount.AutomountServiceAccountToken {
		t.Fatalf("Prometheus ServiceAccount = %#v", serviceAccount)
	}

	var role rbacv1.Role
	if err := yaml.Unmarshal([]byte(documents[1]), &role); err != nil {
		t.Fatalf("decode Prometheus Role: %v", err)
	}

	if len(role.Rules) != 1 ||
		!slices.Equal(role.Rules[0].Resources, []string{"pods"}) ||
		!slices.Equal(role.Rules[0].Verbs, []string{"get", "list", "watch"}) {
		t.Fatalf("Prometheus Role = %#v", role)
	}

	var metricsService corev1.Service
	if err := yaml.Unmarshal([]byte(documents[3]), &metricsService); err != nil {
		t.Fatalf("decode metrics Service: %v", err)
	}

	if metricsService.Annotations["prometheus.io/scrape"] != "true" ||
		metricsService.Spec.Ports[0].TargetPort.StrVal != "metrics" {
		t.Fatalf("metrics Service = %#v", metricsService)
	}

	var config corev1.ConfigMap
	if err := yaml.Unmarshal([]byte(documents[4]), &config); err != nil {
		t.Fatalf("decode Prometheus ConfigMap: %v", err)
	}

	assertContainsAll(
		t,
		config.Data["prometheus.yml"],
		"scrape_interval: 15s",
		"project-signal-controller-metrics.signal-ns.svc:8080",
		"job_name: project-signal-live-workloads",
		"scrape_interval: 5s",
		"role: pod",
		"__meta_kubernetes_pod_annotation_prometheus_io_scrape",
		"127.0.0.1:9090",
	)

	var deployment appsv1.Deployment
	if err := yaml.Unmarshal([]byte(documents[5]), &deployment); err != nil {
		t.Fatalf("decode Prometheus Deployment: %v", err)
	}

	container := deployment.Spec.Template.Spec.Containers[0]
	if container.Image != testPrometheusImage ||
		!slices.Contains(container.Args, "--storage.tsdb.retention.time=24h") ||
		!slices.Contains(container.Args, "--storage.tsdb.retention.size=1GB") ||
		deployment.Spec.Template.Spec.ServiceAccountName != "project-signal-prometheus" ||
		deployment.Spec.Template.Spec.AutomountServiceAccountToken == nil ||
		!*deployment.Spec.Template.Spec.AutomountServiceAccountToken {
		t.Fatalf("Prometheus Deployment = %#v", deployment.Spec.Template.Spec)
	}

	podSecurityContext := deployment.Spec.Template.Spec.SecurityContext

	containerSecurityContext := container.SecurityContext
	if podSecurityContext == nil ||
		podSecurityContext.RunAsNonRoot == nil ||
		!*podSecurityContext.RunAsNonRoot ||
		podSecurityContext.RunAsUser == nil ||
		*podSecurityContext.RunAsUser != 65534 ||
		containerSecurityContext == nil ||
		containerSecurityContext.AllowPrivilegeEscalation == nil ||
		*containerSecurityContext.AllowPrivilegeEscalation ||
		containerSecurityContext.ReadOnlyRootFilesystem == nil ||
		!*containerSecurityContext.ReadOnlyRootFilesystem ||
		!slices.Contains(containerSecurityContext.Capabilities.Drop, corev1.Capability("ALL")) {
		t.Fatalf(
			"Prometheus security contexts = pod %#v, container %#v",
			podSecurityContext,
			containerSecurityContext,
		)
	}

	if deployment.Spec.Template.Spec.Volumes[1].EmptyDir == nil ||
		deployment.Spec.Template.Spec.Volumes[1].EmptyDir.SizeLimit == nil ||
		deployment.Spec.Template.Spec.Volumes[1].EmptyDir.SizeLimit.String() != "1Gi" {
		t.Fatalf("Prometheus data volume = %#v", deployment.Spec.Template.Spec.Volumes)
	}

	var queryService corev1.Service
	if err := yaml.Unmarshal([]byte(documents[6]), &queryService); err != nil {
		t.Fatalf("decode Prometheus query Service: %v", err)
	}

	if queryService.Spec.Type != corev1.ServiceTypeClusterIP ||
		queryService.Spec.Ports[0].Port != 9090 ||
		queryService.Spec.Ports[0].TargetPort.StrVal != "web" {
		t.Fatalf("Prometheus query Service = %#v", queryService.Spec)
	}
}

func TestPrometheusRenderRejectsMutableImage(t *testing.T) {
	t.Parallel()

	data := signalRenderData()
	data["PrometheusImage"] = "quay.io/prometheus/prometheus:latest"

	err := render.Render(".", t.TempDir(), data)
	if err == nil || !strings.Contains(err.Error(), "PrometheusImage must be an immutable sha256 digest reference") {
		t.Fatalf("render error = %v", err)
	}
}

func TestRenderedGPUInventoryCatalogPinsSource(t *testing.T) {
	t.Parallel()

	output := renderSignal(t, signalRenderData())
	documents := splitYAMLDocuments(string(readRenderedFile(t, output, "05-gpu-inventory-tests.yaml")))

	if len(documents) != 1 {
		t.Fatalf("GPU inventory documents = %d, want 1", len(documents))
	}

	var test unstructured.Unstructured
	if err := yaml.Unmarshal([]byte(documents[0]), &test.Object); err != nil {
		t.Fatalf("decode CertificationTest: %v", err)
	}

	if test.GetKind() != "CertificationTest" ||
		test.GetLabels()["signal.unbounded-cloud.io/catalog-role"] != "qualification" {
		t.Fatalf("CertificationTest identity = %s/%v", test.GetKind(), test.GetLabels())
	}

	artifacts, found, err := unstructured.NestedSlice(test.Object, "spec", "artifacts")
	if err != nil || !found || len(artifacts) != 1 {
		t.Fatalf("CertificationTest artifacts = %#v, found %t, error %v", artifacts, found, err)
	}

	artifact, ok := artifacts[0].(map[string]any)
	if !ok {
		t.Fatalf("CertificationTest artifact = %#v", artifacts[0])
	}

	script, _ := artifact["content"].(string)

	scriptDigest, _ := artifact["digest"].(string)
	if artifact["name"] != "gpu-inventory.sh" {
		t.Fatalf("CertificationTest artifact name = %v, want gpu-inventory.sh", artifact["name"])
	}

	scriptSum := sha256.Sum256([]byte(script))
	if got := fmt.Sprintf("sha256:%x", scriptSum); got != scriptDigest || got != testScriptDigest {
		t.Fatalf(
			"GPU inventory script digest = %s, artifact = %s, test fixture = %s",
			got,
			scriptDigest,
			testScriptDigest,
		)
	}

	assertContainsAll(
		t,
		script,
		`printf '\000'`,
		`printf '%s' "$gpu_uuid"`,
	)

	if strings.Contains(script, `printf '%s\000' "$gpu_uuid"`) {
		t.Fatal("GPU inventory digest includes a trailing NUL separator")
	}

	gotArtifact, found, err := unstructured.NestedString(
		test.Object,
		"spec",
		"driver",
		"script",
		"artifact",
	)
	if err != nil || !found || gotArtifact != "gpu-inventory.sh" {
		t.Fatalf(
			"CertificationTest script artifact = %q, found %t, error %v",
			gotArtifact,
			found,
			err,
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
			"CertificationTest GPU request = %q, found %t, error %v, want 8",
			gotGPU,
			found,
			err,
		)
	}
}

func TestRenderedGPUInventoryScriptProducesCanonicalFingerprints(t *testing.T) {
	t.Parallel()

	script := renderedGPUInventoryScript(t)
	result := runGPUInventoryScript(t, script, gpuInventoryFixture{
		gpuCount: 3,
		startDrivers: strings.Join([]string{
			"590.48.01",
			"590.48.01",
			"590.48.01",
		}, "\n"),
		startInventory: strings.Join([]string{
			"2, GPU-cccccccc-cccc-cccc-cccc-cccccccccccc",
			"0, GPU-aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
			"1, GPU-bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb",
		}, "\n"),
	})

	if result.Outcome != "Passed" {
		t.Fatalf("GPU inventory outcome = %q, want Passed", result.Outcome)
	}

	if len(result.Participants) != 1 {
		t.Fatalf("GPU inventory participants = %d, want 1", len(result.Participants))
	}

	participant := result.Participants[0]

	const wantDigest = "sha256:b553224e314b1f8192da92db1febdbcd2bad39ab8bf106fb1da46cf148920cf8"

	if participant.Start.DriverVersion != "590.48.01" ||
		participant.End.DriverVersion != participant.Start.DriverVersion ||
		participant.Start.GPUInventoryDigest != wantDigest ||
		participant.End.GPUInventoryDigest != wantDigest {
		t.Fatalf("GPU inventory fingerprint = %#v", participant)
	}

	wantDevices := []string{"gpu-index-0", "gpu-index-1", "gpu-index-2"}
	if !slices.Equal(participant.VisibleDevices, wantDevices) ||
		!slices.Equal(participant.CoveredDevices, wantDevices) {
		t.Fatalf(
			"GPU inventory devices = visible %v, covered %v, want %v",
			participant.VisibleDevices,
			participant.CoveredDevices,
			wantDevices,
		)
	}

	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("encode GPU inventory result: %v", err)
	}

	if bytes.Contains(encoded, []byte("GPU-aaaaaaaa")) {
		t.Fatalf("GPU inventory result exposes a raw GPU UUID: %s", encoded)
	}
}

func TestRenderedGPUInventoryScriptFailsClosed(t *testing.T) {
	t.Parallel()

	script := renderedGPUInventoryScript(t)
	validDrivers := "590.48.01\n590.48.01\n590.48.01"
	validInventory := strings.Join([]string{
		"2, GPU-cccccccc-cccc-cccc-cccc-cccccccccccc",
		"0, GPU-aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
		"1, GPU-bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb",
	}, "\n")

	tests := []struct {
		name    string
		fixture gpuInventoryFixture
	}{
		{
			name: "mixed driver versions",
			fixture: gpuInventoryFixture{
				gpuCount:       3,
				startDrivers:   "590.48.01\n590.48.02\n590.48.01",
				startInventory: validInventory,
			},
		},
		{
			name: "missing driver version",
			fixture: gpuInventoryFixture{
				gpuCount:       3,
				startDrivers:   "590.48.01\n590.48.01",
				startInventory: validInventory,
			},
		},
		{
			name: "duplicate index",
			fixture: gpuInventoryFixture{
				gpuCount:     3,
				startDrivers: validDrivers,
				startInventory: strings.Join([]string{
					"0, GPU-aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
					"0, GPU-bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb",
					"2, GPU-cccccccc-cccc-cccc-cccc-cccccccccccc",
				}, "\n"),
			},
		},
		{
			name: "duplicate UUID",
			fixture: gpuInventoryFixture{
				gpuCount:     3,
				startDrivers: validDrivers,
				startInventory: strings.Join([]string{
					"0, GPU-aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
					"1, GPU-aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
					"2, GPU-cccccccc-cccc-cccc-cccc-cccccccccccc",
				}, "\n"),
			},
		},
		{
			name: "gapped index",
			fixture: gpuInventoryFixture{
				gpuCount:     3,
				startDrivers: validDrivers,
				startInventory: strings.Join([]string{
					"0, GPU-aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
					"2, GPU-bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb",
					"10, GPU-cccccccc-cccc-cccc-cccc-cccccccccccc",
				}, "\n"),
			},
		},
		{
			name: "noncanonical index",
			fixture: gpuInventoryFixture{
				gpuCount:     3,
				startDrivers: validDrivers,
				startInventory: strings.Join([]string{
					"00, GPU-aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
					"1, GPU-bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb",
					"2, GPU-cccccccc-cccc-cccc-cccc-cccccccccccc",
				}, "\n"),
			},
		},
		{
			name: "missing UUID",
			fixture: gpuInventoryFixture{
				gpuCount:     3,
				startDrivers: validDrivers,
				startInventory: strings.Join([]string{
					"0, ",
					"1, GPU-bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb",
					"2, GPU-cccccccc-cccc-cccc-cccc-cccccccccccc",
				}, "\n"),
			},
		},
		{
			name: "malformed UUID",
			fixture: gpuInventoryFixture{
				gpuCount:     3,
				startDrivers: validDrivers,
				startInventory: strings.Join([]string{
					"0, GPU-not-a-uuid",
					"1, GPU-bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb",
					"2, GPU-cccccccc-cccc-cccc-cccc-cccccccccccc",
				}, "\n"),
			},
		},
		{
			name: "start end digest mismatch",
			fixture: gpuInventoryFixture{
				gpuCount:       3,
				startDrivers:   validDrivers,
				startInventory: validInventory,
				endInventory: strings.Join([]string{
					"0, GPU-dddddddd-dddd-dddd-dddd-dddddddddddd",
					"1, GPU-bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb",
					"2, GPU-cccccccc-cccc-cccc-cccc-cccccccccccc",
				}, "\n"),
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			result := runGPUInventoryScript(t, script, test.fixture)
			if result.Outcome == "Passed" {
				t.Fatalf("GPU inventory outcome = Passed, want fail-closed non-pass: %#v", result)
			}
		})
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
	firstTemplate := readRenderedFile(t, firstOutput, "08-vllm-canary-workflowtemplate.yaml")

	var workflow unstructured.Unstructured
	if err := yaml.Unmarshal(firstTemplate, &workflow.Object); err != nil {
		t.Fatalf("decode vLLM WorkflowTemplate: %v", err)
	}

	spec, found, err := unstructured.NestedMap(workflow.Object, "spec")
	if err != nil || !found {
		t.Fatalf("read vLLM WorkflowTemplate spec: found %t, error %v", found, err)
	}

	templates, found, err := unstructured.NestedSlice(spec, "templates")
	if err != nil || !found || len(templates) != 1 {
		t.Fatalf("vLLM WorkflowTemplate templates = %#v, found %t, error %v", templates, found, err)
	}

	canary, ok := templates[0].(map[string]any)
	if !ok {
		t.Fatalf("vLLM WorkflowTemplate canary = %#v", templates[0])
	}

	if _, hasContainer := canary["container"]; !hasContainer {
		t.Fatal("vLLM WorkflowTemplate does not define exactly one container template")
	}

	if _, hasContainers := canary["containers"]; hasContainers {
		t.Fatal("vLLM WorkflowTemplate defines a container set instead of one container")
	}

	retryStrategy, ok := canary["retryStrategy"].(map[string]any)
	if !ok || retryStrategy["limit"] != "0" {
		t.Fatalf("vLLM WorkflowTemplate retry strategy = %#v, want limit 0", retryStrategy)
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
		readRenderedFile(t, secondOutput, "08-vllm-canary-workflowtemplate.yaml"),
	) {
		t.Fatal("vLLM WorkflowTemplate changed when only its recorded digest changed")
	}

	templateText := string(firstTemplate)
	for _, value := range []string{
		"primary-participant-node-name",
		"test-json",
		"resolved-inputs-json",
		"result-configmap",
		"serviceAccountName: project-signal-vllm-workload",
		"automountServiceAccountToken: true",
		"subPath: EleutherAI--pythia-70m-deduped/snapshots/e93a9faa9c77e5d09219f6c868bfc7a1bd65593c",
		"nvidia.com/gpu: \"8\"",
		"mountPath: /dev/shm",
		"medium: Memory",
		"readOnly: true",
	} {
		if !strings.Contains(templateText, value) {
			t.Fatalf("vLLM WorkflowTemplate does not contain %q", value)
		}
	}

	for _, value := range []string{"podGC:", "ttlStrategy:"} {
		if strings.Contains(templateText, value) {
			t.Fatalf("vLLM WorkflowTemplate contains competing cleanup policy %q", value)
		}
	}

	var test unstructured.Unstructured
	if err := yaml.Unmarshal(
		readRenderedFile(t, secondOutput, "09-vllm-canary-test.yaml"),
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

	testText := string(readRenderedFile(t, secondOutput, "09-vllm-canary-test.yaml"))

	var testObject unstructured.Unstructured
	if err := yaml.Unmarshal([]byte(testText), &testObject); err != nil {
		t.Fatalf("decode vLLM CertificationTest: %v", err)
	}

	metrics, found, err := unstructured.NestedSlice(testObject.Object, "spec", "metrics")
	if err != nil || !found {
		t.Fatalf("read vLLM CertificationTest metrics: found=%t err=%v", found, err)
	}

	if len(metrics) != 16 {
		t.Fatalf("vLLM CertificationTest metric count = %d, want schema maximum 16", len(metrics))
	}

	for _, metric := range []string{
		"serviceReady",
		"modelIdentity",
		"modelRevision",
		"modelWeightsDigest",
		"requestSucceeded",
		"promptTokens",
		"completionTokens",
		"errorSourceCounts",
		"restartSourceCounts",
		"tensorParallelSize",
		"observedGpuCount",
		"activeGpuCount",
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

func TestRenderedVLLMModelCacheSeedsVerifiedOfflinePVC(t *testing.T) {
	t.Parallel()

	data := signalRenderData()
	data["VLLMCanaryEnabled"] = "true"
	data["VLLMCanaryImage"] = "example.invalid/vllm-canary@sha256:" + strings.Repeat("f", 64)
	data["VLLMCanaryCPU"] = "4"
	data["VLLMCanaryMemory"] = "16Gi"
	data["VLLMModelCachePVC"] = "approved-model-cache"
	data["VLLMModelCacheSeedEnabled"] = "true"
	data["VLLMModelCacheImage"] = "example.invalid/vllm-cache@sha256:" + strings.Repeat("9", 64)
	data["VLLMModelCacheSeedID"] = "52b9b0fa1a8a"
	data["VLLMModelCacheCapacity"] = "1Gi"
	data["VLLMModelCacheStorageClass"] = "approved-storage"

	output := renderSignal(t, data)

	content := string(readRenderedFile(t, output, "07-vllm-model-cache.yaml"))
	if _, err := os.Stat(filepath.Join(output, "08-vllm-canary-workflowtemplate.yaml")); err != nil {
		t.Fatalf("runtime WorkflowTemplate is not packaged after the cache seed manifest: %v", err)
	}

	assertContainsAll(
		t,
		content,
		"vLLM model cache",
		"kind: PersistentVolumeClaim",
		"name: approved-model-cache",
		"accessModes: [ReadWriteOnce]",
		"storageClassName: approved-storage",
		"kind: Job",
		"serviceAccountName: project-signal-vllm-cache-seeder",
		"automountServiceAccountToken: false",
		"--source-dir=/model",
		"--target-dir=/cache/EleutherAI--pythia-70m-deduped/snapshots/e93a9faa9c77e5d09219f6c868bfc7a1bd65593c",
		"--manifest-digest=sha256:52b9b0fa1a8a67424d2e8f7994675698c60e3eb1a7925091930ecc4958dbcd7e",
		"--seed-uid=$(POD_UID)",
		"readOnlyRootFilesystem: true",
	)
}

func TestVLLMModelCacheSeedFailsClosedWithoutImmutableInputs(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name  string
		key   string
		value string
	}{
		{name: "mutable image", key: "VLLMModelCacheImage", value: "example.invalid/cache:latest"},
		{name: "unsafe subpath", key: "VLLMModelCacheSubPath", value: "../model"},
		{name: "missing capacity", key: "VLLMModelCacheCapacity", value: ""},
		{name: "missing storage class", key: "VLLMModelCacheStorageClass", value: ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			data := signalRenderData()
			data["VLLMCanaryEnabled"] = "true"
			data["VLLMCanaryImage"] = "example.invalid/vllm-canary@sha256:" + strings.Repeat("f", 64)
			data["VLLMCanaryCPU"] = "4"
			data["VLLMCanaryMemory"] = "16Gi"
			data["VLLMModelCachePVC"] = "approved-model-cache"
			data["VLLMModelCacheSeedEnabled"] = "true"
			data["VLLMModelCacheImage"] = "example.invalid/vllm-cache@sha256:" + strings.Repeat("9", 64)
			data["VLLMModelCacheSeedID"] = "52b9b0fa1a8a"
			data["VLLMModelCacheCapacity"] = "1Gi"
			data["VLLMModelCacheStorageClass"] = "approved-storage"
			data[tt.key] = tt.value

			output := t.TempDir()
			if err := render.Render(".", output, data); err == nil {
				t.Fatal("render accepted an unsafe vLLM model cache input")
			}
		})
	}
}

func TestRenderedAcceptanceSuitesIncludeQualificationAndVLLM(t *testing.T) {
	t.Parallel()

	data := signalRenderData()
	data["AcceptanceSuitesEnabled"] = "true"
	data["GPUInventoryTestUID"] = "11111111-1111-1111-1111-111111111111"
	data["GPUInventoryTestGeneration"] = "1"
	data["GPUInventoryTestContentDigest"] = testDigest
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
	documents := splitYAMLDocuments(string(readRenderedFile(t, output, "10-acceptance-suites.yaml")))

	if len(documents) != 2 {
		t.Fatalf("acceptance Suite documents = %d, want 2", len(documents))
	}

	var baseline unstructured.Unstructured
	if err := yaml.Unmarshal([]byte(documents[0]), &baseline.Object); err != nil {
		t.Fatalf("decode baseline Suite: %v", err)
	}

	baselinePhases, found, err := unstructured.NestedSlice(baseline.Object, "spec", "phases")
	if err != nil || !found || len(baselinePhases) != 1 {
		t.Fatalf("baseline phases = %d, found %t, error %v, want 1", len(baselinePhases), found, err)
	}

	if strings.Count(documents[0], "name: project-signal-gpu-inventory-v1") != 1 {
		t.Fatal("baseline Suite must contain exactly one canonical qualification Test")
	}

	if !strings.Contains(documents[0], "metric: driverVersionSet") ||
		!strings.Contains(documents[0], "stringValue: consistent") {
		t.Fatal("baseline Suite must require a consistent driver version")
	}

	vllmText := documents[1]
	for _, metric := range []string{
		"serviceReady",
		"modelIdentity",
		"modelRevision",
		"modelWeightsDigest",
		"requestSucceeded",
		"promptTokens",
		"completionTokens",
		"errorSourceCounts",
		"restartSourceCounts",
		"tensorParallelSize",
		"observedGpuCount",
		"activeGpuCount",
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

func TestRenderedExecutionAuthorityIsExplicitAndFailClosed(t *testing.T) {
	t.Parallel()

	data := signalRenderData()
	enableExecutionAuthorityRender(data)

	output := renderSignal(t, data)

	documents := splitYAMLDocuments(string(readRenderedFile(t, output, "11-execution-authority.yaml")))
	if len(documents) != 8 {
		t.Fatalf("execution authority documents = %d, want 8", len(documents))
	}

	text := strings.Join(documents, "\n---\n")
	assertContainsAll(
		t,
		text,
		"acceptanceSuitesEnabled: \"true\"",
		"cutoverApproved: \"true\"",
		"hardAbort: \"true\"",
		"namespace: signal-authority",
		"resourceNames:",
		"- project-signal-execution-authority-aaaaaaaaaaaa-dddddddddddd",
		"- project-signal-execution-state",
		"uid: 11111111-1111-1111-1111-111111111111",
		"uid: 33333333-3333-3333-3333-333333333333",
		"pvc://signal-ns/approved-model-cache@"+data["VLLMModelCacheManifestDigest"],
		"name: canonical-gpu-inventory",
		"name: offline-vllm-canary",
		"creditRule: PerParticipant",
		"mode: Current",
		"nonPassBehavior: Revoke",
		"mode: None",
	)

	transition := string(readRenderedFile(t, output, "12-execution-authority-transition.yaml"))
	assertContainsAll(
		t,
		transition,
		`"op": "test"`,
		`"value": "true"`,
		`"op": "replace"`,
		`"value": "false"`,
	)

	deployment := string(readRenderedFile(t, output, "06-deployment.yaml"))
	assertContainsAll(
		t,
		deployment,
		"--authority-namespace=signal-authority",
		"--authority-name=project-signal-execution-authority-aaaaaaaaaaaa-dddddddddddd",
		"--static-package-digest="+testDigest,
	)
}

func TestRenderedBaselineOnlyAuthorityOmitsVLLM(t *testing.T) {
	t.Parallel()

	data := signalRenderData()
	enableBaselineExecutionAuthorityRender(data)

	output := renderSignal(t, data)
	authority := string(readRenderedFile(t, output, "11-execution-authority.yaml"))

	documents := splitYAMLDocuments(authority)
	if len(documents) != 7 {
		t.Fatalf("baseline-only authority documents = %d, want 7", len(documents))
	}

	entries, err := os.ReadDir(output)
	if err != nil {
		t.Fatalf("read baseline-only render directory: %v", err)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		content := strings.ToLower(string(readRenderedFile(t, output, entry.Name())))
		if strings.Contains(content, "project-signal-vllm") {
			t.Fatalf("baseline-only render %s contains deferred vLLM resource", entry.Name())
		}
	}

	rbac := string(readRenderedFile(t, output, "03-adapter-serviceaccount-rbac.yaml"))
	for _, deferredPermission := range []string{
		`resources: ["persistentvolumeclaims"]`,
		`resources: ["volumeattachments"]`,
	} {
		if strings.Contains(rbac, deferredPermission) {
			t.Fatalf("baseline-only RBAC contains deferred vLLM permission %q", deferredPermission)
		}
	}

	assertContainsAll(
		t,
		authority,
		"baseline-only authority",
		"name: canonical-gpu-inventory",
		"nonPassBehavior: Revoke",
		"mode: None",
	)
}

func TestExecutionAuthorityRenderRejectsUnsafeTransition(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name  string
		key   string
		value string
		error string
	}{
		{
			name:  "suites disabled",
			key:   "AcceptanceSuitesEnabled",
			value: "false",
			error: "AcceptanceSuitesEnabled=true",
		},
		{
			name:  "cutover not approved",
			key:   "CutoverApproved",
			value: "false",
			error: "CutoverApproved=true",
		},
		{
			name:  "unversioned immutable authority",
			key:   "AuthorityName",
			value: "project-signal-execution-authority",
			error: "AuthorityName must match",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			data := signalRenderData()
			enableExecutionAuthorityRender(data)
			data[tt.key] = tt.value

			err := render.Render(".", t.TempDir(), data)
			if err == nil || !strings.Contains(err.Error(), tt.error) {
				t.Fatalf("render error = %v, want %q", err, tt.error)
			}
		})
	}
}

func TestEnabledVLLMAuthorityRequiresCompleteBinding(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name  string
		key   string
		error string
	}{
		{
			name:  "profile UID",
			key:   "VLLMProfileUID",
			error: "VLLMProfileUID must be the exact live Profile UID",
		},
		{
			name:  "model cache PVC",
			key:   "VLLMModelCachePVC",
			error: "VLLMModelCachePVC is required",
		},
		{
			name:  "model cache digest",
			key:   "VLLMModelCacheManifestDigest",
			error: "VLLMModelCacheManifestDigest must be",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			data := signalRenderData()
			enableExecutionAuthorityRender(data)
			data[tt.key] = ""

			err := render.Render(".", t.TempDir(), data)
			if err == nil || !strings.Contains(err.Error(), tt.error) {
				t.Fatalf("render error = %v, want %q", err, tt.error)
			}
		})
	}
}

func TestExecutionAuthorityRejectsSharedAuthorityNamespace(t *testing.T) {
	t.Parallel()

	data := signalRenderData()
	enableExecutionAuthorityRender(data)
	data["AuthorityNamespace"] = data["Namespace"]

	err := render.Render(".", t.TempDir(), data)
	if err == nil || !strings.Contains(err.Error(), "AuthorityNamespace must be distinct") {
		t.Fatalf("render error = %v, want distinct authority namespace rejection", err)
	}
}

func TestDisabledExecutionAuthorityClearsControllerBinding(t *testing.T) {
	t.Parallel()

	data := signalRenderData()
	data["StaticPackageDigest"] = testDigest

	output := renderSignal(t, data)
	deployment := string(readRenderedFile(t, output, "06-deployment.yaml"))
	assertContainsAll(
		t,
		deployment,
		"--authority-name=project-signal-execution-authority-disabled",
		"--static-package-digest=",
	)

	if strings.Contains(deployment, "--static-package-digest="+testDigest) {
		t.Fatal("disabled execution authority retained the static package binding")
	}
}

func TestExecutionAuthorityRendersAuditableHardAbort(t *testing.T) {
	t.Parallel()

	data := signalRenderData()
	enableExecutionAuthorityRender(data)
	data["HardAbort"] = "true"

	output := renderSignal(t, data)
	content := string(readRenderedFile(t, output, "11-execution-authority.yaml"))
	assertContainsAll(
		t,
		content,
		"name: project-signal-execution-authority",
		"name: project-signal-execution-state",
		"hardAbort: \"true\"",
	)

	transition := readRenderedFile(t, output, "12-execution-authority-transition.yaml")
	if len(bytes.TrimSpace(transition)) != 0 {
		t.Fatalf("hard-aborted authority rendered a release transition: %s", transition)
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

	data := signalRenderData()
	enableExecutionAuthorityRender(data)

	output := renderSignal(t, data)
	workflow := string(readRenderedFile(t, output, "04-adapter-workflowtemplate.yaml"))
	assertContainsAll(
		t,
		workflow,
		"vLLM-enabled adapter WorkflowTemplate",
		"--workflow-service-account",
		"project-signal-vllm-workload",
	)

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
		"",
		"persistentvolumeclaims",
		[]string{"get"},
	)
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
		[]string{"get", "list", "watch", "create", "delete"},
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
		[]string{"get", "list", "create", "delete"},
	)
	assertRule(
		t,
		adapterDocuments,
		"Role",
		"signal-ns",
		"project-signal-adapter",
		"",
		"pods",
		[]string{"get", "list", "watch", "create", "delete"},
	)
	assertRule(
		t,
		adapterDocuments,
		"Role",
		"signal-ns",
		"project-signal-vllm-workload",
		"",
		"configmaps",
		[]string{"create"},
	)
	assertRule(
		t,
		adapterDocuments,
		"Role",
		"signal-ns",
		"project-signal-vllm-workload",
		"argoproj.io",
		"workflowtaskresults",
		[]string{"create", "patch"},
	)
	assertExactRoleResources(
		t,
		adapterDocuments,
		"signal-ns",
		"project-signal-vllm-workload",
		map[string][]string{
			"/configmaps":                     {"create"},
			"argoproj.io/workflowtaskresults": {"create", "patch"},
		},
	)
	assertRoleBindingSubject(
		t,
		adapterDocuments,
		"signal-ns",
		"project-signal-vllm-workload",
		"project-signal-vllm-workload",
	)
	assertServiceAccountAutomount(
		t,
		adapterDocuments,
		"signal-ns",
		"project-signal-vllm-workload",
		true,
	)
	assertServiceAccountAutomount(
		t,
		adapterDocuments,
		"signal-ns",
		"project-signal-vllm-cache-seeder",
		false,
	)
	assertNoRoleBindingSubject(
		t,
		adapterDocuments,
		"signal-ns",
		"project-signal-vllm-cache-seeder",
	)
	assertRule(
		t,
		adapterDocuments,
		"ClusterRole",
		"",
		"project-signal-adapter-infrastructure-reader",
		"",
		"nodes",
		[]string{"get"},
	)
	assertRule(
		t,
		adapterDocuments,
		"ClusterRole",
		"",
		"project-signal-adapter-infrastructure-reader",
		"signal.unbounded-cloud.io",
		"certificationtests",
		[]string{"get"},
	)
	assertRule(
		t,
		adapterDocuments,
		"ClusterRole",
		"",
		"project-signal-adapter-infrastructure-reader",
		"storage.k8s.io",
		"volumeattachments",
		[]string{"list"},
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

type gpuInventoryFixture struct {
	gpuCount       int
	startDrivers   string
	endDrivers     string
	startInventory string
	endInventory   string
}

type gpuInventoryResult struct {
	Outcome      string `json:"outcome"`
	Participants []struct {
		Start struct {
			DriverVersion      string `json:"driverVersion"`
			GPUInventoryDigest string `json:"gpuInventoryDigest"`
		} `json:"start"`
		End struct {
			DriverVersion      string `json:"driverVersion"`
			GPUInventoryDigest string `json:"gpuInventoryDigest"`
		} `json:"end"`
		VisibleDevices []string `json:"visibleDevices"`
		CoveredDevices []string `json:"coveredDevices"`
	} `json:"participants"`
}

func renderedGPUInventoryScript(t *testing.T) string {
	t.Helper()

	output := renderSignal(t, signalRenderData())
	documents := splitYAMLDocuments(string(readRenderedFile(t, output, "05-gpu-inventory-tests.yaml")))

	var test unstructured.Unstructured
	if err := yaml.Unmarshal([]byte(documents[0]), &test.Object); err != nil {
		t.Fatalf("decode GPU inventory CertificationTest: %v", err)
	}

	artifacts, found, err := unstructured.NestedSlice(test.Object, "spec", "artifacts")
	if err != nil || !found || len(artifacts) != 1 {
		t.Fatalf("CertificationTest artifacts = %#v, found %t, error %v", artifacts, found, err)
	}

	artifact, ok := artifacts[0].(map[string]any)
	if !ok {
		t.Fatalf("CertificationTest artifact = %#v", artifacts[0])
	}

	script, ok := artifact["content"].(string)
	if !ok || script == "" {
		t.Fatalf("CertificationTest artifact content = %#v", artifact["content"])
	}

	return script
}

func runGPUInventoryScript(
	t *testing.T,
	script string,
	fixture gpuInventoryFixture,
) gpuInventoryResult {
	t.Helper()

	if fixture.endDrivers == "" {
		fixture.endDrivers = fixture.startDrivers
	}

	if fixture.endInventory == "" {
		fixture.endInventory = fixture.startInventory
	}

	work := t.TempDir()

	bin := filepath.Join(work, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatalf("create mock binary directory: %v", err)
	}

	mock := `#!/bin/sh
set -eu

next_fixture() {
  counter_file="$1"
  start_value="$2"
  end_value="$3"
  count=0
  if [ -f "$counter_file" ]; then
    count="$(cat "$counter_file")"
  fi
  printf '%s' "$((count + 1))" >"$counter_file"
  if [ "$count" -eq 0 ]; then
    printf '%s\n' "$start_value"
  else
    printf '%s\n' "$end_value"
  fi
}

case "$*" in
  "-L")
    index=0
    while [ "$index" -lt "$TEST_GPU_COUNT" ]; do
      printf 'GPU %s: Test GPU (UUID: redacted)\n' "$index"
      index=$((index + 1))
    done
    ;;
  "--query-gpu=driver_version --format=csv,noheader,nounits")
    next_fixture "$TEST_STATE/driver-count" "$TEST_START_DRIVERS" "$TEST_END_DRIVERS"
    ;;
  "--query-gpu=index,uuid --format=csv,noheader,nounits")
    next_fixture "$TEST_STATE/inventory-count" "$TEST_START_INVENTORY" "$TEST_END_INVENTORY"
    ;;
  *)
    exit 2
    ;;
esac
`

	mockPath := filepath.Join(bin, "nvidia-smi")
	if err := os.WriteFile(mockPath, []byte(mock), 0o755); err != nil {
		t.Fatalf("write mock nvidia-smi: %v", err)
	}

	scriptPath := filepath.Join(work, "gpu-inventory.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write GPU inventory script: %v", err)
	}

	command := exec.Command("/bin/sh", scriptPath)

	command.Env = append(
		os.Environ(),
		"PATH="+bin+":"+os.Getenv("PATH"),
		fmt.Sprintf("TEST_GPU_COUNT=%d", fixture.gpuCount),
		"TEST_STATE="+work,
		"TEST_START_DRIVERS="+fixture.startDrivers,
		"TEST_END_DRIVERS="+fixture.endDrivers,
		"TEST_START_INVENTORY="+fixture.startInventory,
		"TEST_END_INVENTORY="+fixture.endInventory,
		"PROJECT_SIGNAL_INVOCATION_ID=invocation-1",
		"PROJECT_SIGNAL_RUN_UID=run-uid",
		"PROJECT_SIGNAL_STAGE_ID=stage-1",
		"PROJECT_SIGNAL_STAGE_ORDER=0",
		"PROJECT_SIGNAL_PLAN_DIGEST="+testDigest,
		"PROJECT_SIGNAL_TEST_NAME=project-signal-gpu-inventory-v1",
		"PROJECT_SIGNAL_TEST_UID=test-uid",
		"PROJECT_SIGNAL_TEST_GENERATION=1",
		"PROJECT_SIGNAL_TEST_CONTENT_DIGEST="+testDigest,
		"PROJECT_SIGNAL_TEST_TRUST=Approved",
		"PROJECT_SIGNAL_RESOLVED_INPUTS_JSON=[]",
		"PROJECT_SIGNAL_PARTICIPANT_INDEX=0",
		"PROJECT_SIGNAL_PARTICIPANT_ROLE=worker",
		"PROJECT_SIGNAL_NODE_UID=node-uid",
	)

	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("run GPU inventory script: %v\n%s", err, output)
	}

	const prefix = "PROJECT_SIGNAL_INVOCATION_RESULT_V1 "

	line := strings.TrimSpace(string(output))
	if !strings.HasPrefix(line, prefix) {
		t.Fatalf("GPU inventory output = %q, want invocation result prefix", line)
	}

	var result gpuInventoryResult
	if err := json.Unmarshal([]byte(strings.TrimPrefix(line, prefix)), &result); err != nil {
		t.Fatalf("decode GPU inventory result: %v\n%s", err, line)
	}

	return result
}

func signalRenderData() map[string]string {
	return map[string]string{
		"Namespace":                    "signal-ns",
		"ScriptNamespace":              "script-ns",
		"AuthorityNamespace":           "signal-authority",
		"AuthorityName":                "project-signal-execution-authority",
		"ControllerImage":              testControllerImage,
		"AdapterImage":                 testAdapterImage,
		"RunnerImage":                  testRunnerImage,
		"PrometheusImage":              testPrometheusImage,
		"ResolverImages":               testResolverImage,
		"GPUInventoryScriptDigest":     testScriptDigest,
		"VLLMCanaryEnabled":            "false",
		"VLLMModelCacheSeedEnabled":    "false",
		"VLLMModelCacheSubPath":        "EleutherAI--pythia-70m-deduped/snapshots/e93a9faa9c77e5d09219f6c868bfc7a1bd65593c",
		"VLLMModelCacheManifestDigest": "sha256:52b9b0fa1a8a67424d2e8f7994675698c60e3eb1a7925091930ecc4958dbcd7e",
		"AcceptanceSuitesEnabled":      "false",
		"ExecutionAuthorityEnabled":    "false",
		"CutoverApproved":              "false",
		"HardAbort":                    "true",
		"VLLMWorkflowTemplateDigest":   testDigest,
		"WorkflowTemplateDigest":       testDigest,
		"QualificationGateProtected":   "false",
	}
}

func enableExecutionAuthorityRender(data map[string]string) {
	enableBaselineExecutionAuthorityRender(data)
	data["VLLMCanaryEnabled"] = "true"
	data["VLLMProfileUID"] = "33333333-3333-3333-3333-333333333333"
	data["VLLMCanaryImage"] = "example.invalid/vllm-canary@sha256:" + strings.Repeat("f", 64)
	data["VLLMCanaryCPU"] = "4"
	data["VLLMCanaryMemory"] = "16Gi"
	data["VLLMModelCachePVC"] = "approved-model-cache"
	data["VLLMTestUID"] = "33333333-3333-3333-3333-333333333333"
	data["VLLMTestGeneration"] = "1"
	data["VLLMTestContentDigest"] = testDigest
	data["VLLMModelIdentity"] = "approved-model@sha256:" + strings.Repeat("9", 64)
	data["VLLMModelRevision"] = strings.Repeat("7", 40)
	data["VLLMModelWeightsDigest"] = "sha256:" + strings.Repeat("6", 64)
	data["VLLMPromptFixtureDigest"] = "sha256:" + strings.Repeat("8", 64)
	data["VLLMRequestTimeout"] = "2m"
	data["VLLMInvocationTimeout"] = "30m"
}

func enableBaselineExecutionAuthorityRender(data map[string]string) {
	data["AcceptanceSuitesEnabled"] = "true"
	data["ExecutionAuthorityEnabled"] = "true"
	data["CutoverApproved"] = "true"
	data["HardAbort"] = "false"
	data["SourceCommit"] = strings.Repeat("a", 40)
	data["StaticPackageDigest"] = testDigest
	data["AuthorityName"] = "project-signal-execution-authority-aaaaaaaaaaaa-dddddddddddd"
	data["BaselineProfileUID"] = "11111111-1111-1111-1111-111111111111"
	data["GPUInventoryTestUID"] = "11111111-1111-1111-1111-111111111111"
	data["GPUInventoryTestGeneration"] = "1"
	data["GPUInventoryTestContentDigest"] = testDigest
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

func assertExactRoleResources(
	t *testing.T,
	documents []string,
	namespace string,
	name string,
	want map[string][]string,
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

		if object.Kind != "Role" || object.Metadata.Name != name || object.Metadata.Namespace != namespace {
			continue
		}

		got := make(map[string][]string, len(object.Rules))
		for _, rule := range object.Rules {
			if len(rule.APIGroups) != 1 || len(rule.Resources) != 1 {
				t.Fatalf("Role/%s has non-atomic rule: %#v", name, rule)
			}

			got[rule.APIGroups[0]+"/"+rule.Resources[0]] = rule.Verbs
		}

		if !reflect.DeepEqual(got, want) {
			t.Fatalf("Role/%s rules = %#v, want %#v", name, got, want)
		}

		return
	}

	t.Fatalf("Role/%s in namespace %q not found", name, namespace)
}

func assertRoleBindingSubject(
	t *testing.T,
	documents []string,
	namespace string,
	name string,
	serviceAccount string,
) {
	t.Helper()

	found := false

	for _, document := range documents {
		var object rbacv1.RoleBinding
		if err := yaml.Unmarshal([]byte(document), &object); err != nil {
			t.Fatalf("decode RBAC document: %v", err)
		}

		if object.Kind != "RoleBinding" && object.Kind != "ClusterRoleBinding" {
			continue
		}

		for _, subject := range object.Subjects {
			if subject.Kind != "ServiceAccount" ||
				subject.Name != serviceAccount ||
				subject.Namespace != namespace {
				continue
			}

			if object.Kind != "RoleBinding" ||
				object.Name != name ||
				object.Namespace != namespace ||
				object.RoleRef.Kind != "Role" ||
				object.RoleRef.Name != name ||
				len(object.Subjects) != 1 {
				t.Fatalf("ServiceAccount/%s has unexpected binding: %#v", serviceAccount, object)
			}

			found = true
		}
	}

	if !found {
		t.Fatalf("RoleBinding/%s in namespace %q not found", name, namespace)
	}
}

func assertNoRoleBindingSubject(
	t *testing.T,
	documents []string,
	namespace string,
	serviceAccount string,
) {
	t.Helper()

	for _, document := range documents {
		var object rbacv1.RoleBinding
		if err := yaml.Unmarshal([]byte(document), &object); err != nil {
			t.Fatalf("decode RBAC document: %v", err)
		}

		if object.Kind != "RoleBinding" && object.Kind != "ClusterRoleBinding" {
			continue
		}

		for _, subject := range object.Subjects {
			if subject.Kind == "ServiceAccount" &&
				subject.Name == serviceAccount &&
				subject.Namespace == namespace {
				t.Fatalf("ServiceAccount/%s has unexpected binding: %#v", serviceAccount, object)
			}
		}
	}
}

func assertServiceAccountAutomount(
	t *testing.T,
	documents []string,
	namespace string,
	name string,
	want bool,
) {
	t.Helper()

	for _, document := range documents {
		var object corev1.ServiceAccount
		if err := yaml.Unmarshal([]byte(document), &object); err != nil {
			t.Fatalf("decode ServiceAccount document: %v", err)
		}

		if object.Kind != "ServiceAccount" || object.Name != name || object.Namespace != namespace {
			continue
		}

		if object.AutomountServiceAccountToken == nil ||
			*object.AutomountServiceAccountToken != want {
			t.Fatalf(
				"ServiceAccount/%s automount = %v, want %t",
				name,
				object.AutomountServiceAccountToken,
				want,
			)
		}

		return
	}

	t.Fatalf("ServiceAccount/%s in namespace %q not found", name, namespace)
}

func assertContainsAll(t *testing.T, content, name string, values ...string) {
	t.Helper()

	for _, value := range values {
		if !strings.Contains(content, value) {
			t.Fatalf("%s does not contain %q", name, value)
		}
	}
}
