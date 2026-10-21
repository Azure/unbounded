// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signaladapter

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	signalv1 "github.com/Azure/unbounded/api/signal/v1alpha1"
)

func TestRenderOrdinaryPodsPerParticipant(t *testing.T) {
	t.Parallel()

	config := testConfig()
	config.BackendAPIVersion = "v1"
	config.BackendKind = "Pod"
	config.Queue = ""

	participants, tolerations, environment, err := ValidateConfig(config)
	if err != nil {
		t.Fatal(err)
	}

	resources, err := RenderResources(config, participants, tolerations, environment, testWorkflowOwner(), 900)
	if err != nil {
		t.Fatal(err)
	}

	if len(resources) != 2 {
		t.Fatalf("resources = %d, want 2", len(resources))
	}

	for index, resource := range resources {
		if resource.GetKind() != "Pod" {
			t.Fatalf("resource %d kind = %q", index, resource.GetKind())
		}

		nodeName, _, err := unstructured.NestedString(resource.Object, "spec", "nodeSelector", corev1.LabelHostname)
		if err != nil {
			t.Fatal(err)
		}

		if nodeName != participants[index].NodeName {
			t.Fatalf("resource %d node selector = %q", index, nodeName)
		}

		restartPolicy, _, err := unstructured.NestedString(resource.Object, "spec", "restartPolicy")
		if err != nil {
			t.Fatal(err)
		}

		if restartPolicy != string(corev1.RestartPolicyNever) {
			t.Fatalf("resource %d restart policy = %q", index, restartPolicy)
		}

		automount, _, err := unstructured.NestedBool(resource.Object, "spec", "automountServiceAccountToken")
		if err != nil {
			t.Fatal(err)
		}

		if automount {
			t.Fatalf("resource %d automounts a service account token", index)
		}
	}
}

func TestRenderOrdinaryPodWithKueueQueue(t *testing.T) {
	t.Parallel()

	config := testConfig()
	config.BackendAPIVersion = "v1"
	config.BackendKind = "Pod"
	config.Queue = "certification"

	participants, tolerations, environment, err := ValidateConfig(config)
	if err != nil {
		t.Fatal(err)
	}

	resources, err := RenderResources(config, participants, tolerations, environment, testWorkflowOwner(), 900)
	if err != nil {
		t.Fatal(err)
	}

	if queue := resources[0].GetLabels()["kueue.x-k8s.io/queue-name"]; queue != "certification" {
		t.Fatalf("Pod queue label = %q", queue)
	}
}

func TestRenderMPIJobsPerParticipant(t *testing.T) {
	t.Parallel()

	config := testConfig()

	participants, tolerations, environment, err := ValidateConfig(config)
	if err != nil {
		t.Fatal(err)
	}

	resources, err := RenderResources(config, participants, tolerations, environment, testWorkflowOwner(), 1200)
	if err != nil {
		t.Fatal(err)
	}

	for index, resource := range resources {
		if resource.GetAPIVersion() != "kubeflow.org/v2beta1" || resource.GetKind() != "MPIJob" {
			t.Fatalf("resource %d = %s %s", index, resource.GetAPIVersion(), resource.GetKind())
		}

		queue, _, err := unstructured.NestedString(
			resource.Object,
			"metadata",
			"labels",
			"kueue.x-k8s.io/queue-name",
		)
		if err != nil {
			t.Fatal(err)
		}

		if queue != "certification" {
			t.Fatalf("resource %d queue = %q", index, queue)
		}

		activeDeadline, _, err := unstructured.NestedInt64(resource.Object, "spec", "runPolicy", "activeDeadlineSeconds")
		if err != nil {
			t.Fatal(err)
		}

		if activeDeadline != 1200 {
			t.Fatalf("resource %d active deadline = %d", index, activeDeadline)
		}

		cleanPolicy, _, err := unstructured.NestedString(resource.Object, "spec", "runPolicy", "cleanPodPolicy")
		if err != nil {
			t.Fatal(err)
		}

		if cleanPolicy != "All" {
			t.Fatalf("resource %d cleanPodPolicy = %q", index, cleanPolicy)
		}

		for _, replica := range []string{"Launcher", "Worker"} {
			restartPolicy, _, err := unstructured.NestedString(
				resource.Object,
				"spec",
				"mpiReplicaSpecs",
				replica,
				"template",
				"spec",
				"restartPolicy",
			)
			if err != nil {
				t.Fatal(err)
			}

			if restartPolicy != string(corev1.RestartPolicyNever) {
				t.Fatalf("resource %d %s restart policy = %q", index, replica, restartPolicy)
			}
		}

		if _, found, err := unstructured.NestedString(resource.Object, "spec", "restartPolicy"); err != nil || found {
			t.Fatalf("resource %d has top-level restartPolicy: found=%t err=%v", index, found, err)
		}

		workerNode, _, err := unstructured.NestedString(
			resource.Object,
			"spec",
			"mpiReplicaSpecs",
			"Worker",
			"template",
			"spec",
			"nodeSelector",
			corev1.LabelHostname,
		)
		if err != nil {
			t.Fatal(err)
		}

		if workerNode != participants[index].NodeName {
			t.Fatalf("resource %d worker node = %q", index, workerNode)
		}

		launcherContainers, found, err := unstructured.NestedSlice(
			resource.Object,
			"spec",
			"mpiReplicaSpecs",
			"Launcher",
			"template",
			"spec",
			"containers",
		)
		if err != nil || !found {
			t.Fatalf("resource %d launcher containers: found=%t err=%v", index, found, err)
		}

		launcherJSON, err := json.Marshal(launcherContainers)
		if err != nil {
			t.Fatal(err)
		}

		if strings.Contains(string(launcherJSON), "nvidia.com/gpu") {
			t.Fatalf("resource %d launcher requests GPUs: %s", index, launcherJSON)
		}
	}
}

func TestRenderMPIJobUsesExplicitLifecycleOverrides(t *testing.T) {
	t.Parallel()

	config := testConfig()
	config.ScriptEnvironmentJSON = `{
			"BACKEND_BACKOFF_LIMIT":"2",
			"BACKEND_TTL_SECONDS_AFTER_FINISHED":"7200"
		}`

	participants, tolerations, environment, err := ValidateConfig(config)
	if err != nil {
		t.Fatal(err)
	}

	resources, err := RenderResources(config, participants, tolerations, environment, testWorkflowOwner(), 1200)
	if err != nil {
		t.Fatal(err)
	}

	backoff, _, err := unstructured.NestedInt64(resources[0].Object, "spec", "runPolicy", "backoffLimit")
	if err != nil {
		t.Fatal(err)
	}

	ttl, _, err := unstructured.NestedInt64(resources[0].Object, "spec", "runPolicy", "ttlSecondsAfterFinished")
	if err != nil {
		t.Fatal(err)
	}

	if backoff != 2 || ttl != 7200 {
		t.Fatalf("run policy backoff=%d ttl=%d", backoff, ttl)
	}
}

func TestRenderWorkloadPolicyOnParticipantContainers(t *testing.T) {
	t.Parallel()

	policy := signalv1.WorkloadPolicy{
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:              resource.MustParse("8"),
				corev1.ResourceMemory:           resource.MustParse("32Gi"),
				corev1.ResourceEphemeralStorage: resource.MustParse("10Gi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:              resource.MustParse("8"),
				corev1.ResourceMemory:           resource.MustParse("32Gi"),
				corev1.ResourceEphemeralStorage: resource.MustParse("20Gi"),
			},
		},
		SecurityContext: &corev1.SecurityContext{
			RunAsUser: ptr(int64(0)),
			Capabilities: &corev1.Capabilities{
				Add: []corev1.Capability{"IPC_LOCK"},
			},
		},
		WorkingDir: "/opt/diagnostic",
		Results:    &corev1.EmptyDirVolumeSource{SizeLimit: quantity("2Gi")},
		Volumes: []signalv1.WorkloadVolume{
			{
				Name:      "host-proc",
				MountPath: "/host/proc",
				ReadOnly:  true,
				HostPath:  &corev1.HostPathVolumeSource{Path: "/proc"},
			},
			{
				Name:      "work",
				MountPath: "/work",
				EmptyDir:  &corev1.EmptyDirVolumeSource{SizeLimit: quantity("8Gi")},
			},
		},
	}

	policyJSON, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}

	config := testConfig()
	config.BackendAPIVersion = "v1"
	config.BackendKind = "Pod"
	config.Queue = ""
	config.GPUResourceCount = "8"
	config.WorkloadPolicyJSON = string(policyJSON)

	participants, tolerations, environment, err := ValidateConfig(config)
	if err != nil {
		t.Fatal(err)
	}

	resources, err := RenderResources(config, participants, tolerations, environment, testWorkflowOwner(), 900)
	if err != nil {
		t.Fatal(err)
	}

	var pod corev1.Pod
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(resources[0].Object, &pod); err != nil {
		t.Fatal(err)
	}

	container := pod.Spec.Containers[0]

	gpuLimit := container.Resources.Limits[corev1.ResourceName("nvidia.com/gpu")]
	if container.WorkingDir != "/opt/diagnostic" ||
		container.Resources.Requests.Cpu().String() != "8" ||
		container.Resources.Limits.Memory().String() != "32Gi" ||
		gpuLimit.String() != "8" ||
		container.SecurityContext == nil ||
		container.SecurityContext.RunAsUser == nil ||
		*container.SecurityContext.RunAsUser != 0 ||
		len(container.SecurityContext.Capabilities.Add) != 1 {
		t.Fatalf("participant container = %#v", container)
	}

	if len(pod.Spec.Volumes) != 4 ||
		pod.Spec.Volumes[1].EmptyDir.SizeLimit.String() != "2Gi" ||
		pod.Spec.Volumes[2].HostPath.Path != "/proc" ||
		pod.Spec.Volumes[3].EmptyDir.SizeLimit.String() != "8Gi" {
		t.Fatalf("participant volumes = %#v", pod.Spec.Volumes)
	}
}

func TestMPIWorkloadPolicyDoesNotLeakOntoLauncher(t *testing.T) {
	t.Parallel()

	policyJSON := `{
			"resources":{"requests":{"cpu":"4"},"limits":{"cpu":"4"}},
			"volumes":[{"name":"host-device","mountPath":"/dev/infiniband","hostPath":{"path":"/dev/infiniband"}}]
		}`
	config := testConfig()
	config.WorkloadPolicyJSON = policyJSON

	participants, tolerations, environment, err := ValidateConfig(config)
	if err != nil {
		t.Fatal(err)
	}

	resources, err := RenderResources(config, participants, tolerations, environment, testWorkflowOwner(), 900)
	if err != nil {
		t.Fatal(err)
	}

	content, err := json.Marshal(resources[0].Object)
	if err != nil {
		t.Fatal(err)
	}

	var job struct {
		Spec struct {
			MPIReplicaSpecs map[string]struct {
				Template corev1.PodTemplateSpec `json:"template"`
			} `json:"mpiReplicaSpecs"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(content, &job); err != nil {
		t.Fatal(err)
	}

	launcher := job.Spec.MPIReplicaSpecs["Launcher"].Template.Spec
	worker := job.Spec.MPIReplicaSpecs["Worker"].Template.Spec

	if len(launcher.Volumes) != 2 || len(launcher.Containers[0].VolumeMounts) != 2 {
		t.Fatalf("launcher received participant policy: %#v", launcher)
	}

	if len(worker.Volumes) != 3 ||
		worker.Volumes[2].HostPath.Path != "/dev/infiniband" ||
		worker.Containers[0].Resources.Requests.Cpu().String() != "4" {
		t.Fatalf("worker policy = %#v", worker)
	}
}

func TestRenderRejectsGPUResourceInWorkloadPolicy(t *testing.T) {
	t.Parallel()

	config := testConfig()
	config.WorkloadPolicyJSON = `{"resources":{"limits":{"nvidia.com/gpu":"2"}}}`

	participants, tolerations, environment, err := ValidateConfig(config)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := RenderResources(
		config,
		participants,
		tolerations,
		environment,
		testWorkflowOwner(),
		900,
	); err == nil || !strings.Contains(err.Error(), "gpu-resource-count") {
		t.Fatalf("RenderResources error = %v", err)
	}
}

func TestParseParticipantsRejectsMalformedIdentity(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"noncontiguous index": `[{"index":1,"nodeName":"node-a","nodeUID":"uid-a"}]`,
		"missing UID":         `[{"index":0,"nodeName":"node-a","nodeUID":""}]`,
		"duplicate node":      `[{"index":0,"nodeName":"node-a","nodeUID":"uid-a"},{"index":1,"nodeName":"node-a","nodeUID":"uid-b"}]`,
		"duplicate UID":       `[{"index":0,"nodeName":"node-a","nodeUID":"uid-a"},{"index":1,"nodeName":"node-b","nodeUID":"uid-a"}]`,
	}

	for name, participantsJSON := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			count := 1
			if strings.Contains(participantsJSON, `"index":1`) && strings.Count(participantsJSON, `"index"`) == 2 {
				count = 2
			}

			if _, err := ParseParticipants(participantsJSON, count, "", ""); err == nil {
				t.Fatal("ParseParticipants succeeded")
			}
		})
	}
}

func TestParseParticipantsRejectsTrailingJSON(t *testing.T) {
	t.Parallel()

	participantsJSON := `[{"index":0,"nodeName":"node-a","nodeUID":"uid-a"}] []`
	if _, err := ParseParticipants(participantsJSON, 1, "", ""); err == nil {
		t.Fatal("ParseParticipants accepted trailing JSON")
	}
}

func TestParseParticipantsCompatibilityFallbacks(t *testing.T) {
	t.Parallel()

	for name, compatibility := range map[string][2]string{
		"request Node": {"node-request", ""},
		"legacy list":  {"", "node-legacy,node-ignored"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			participants, err := ParseParticipants("", 1, compatibility[0], compatibility[1])
			if err != nil {
				t.Fatal(err)
			}

			if len(participants) != 1 || participants[0].NodeName == "" {
				t.Fatalf("participants = %#v", participants)
			}
		})
	}
}

func TestValidateConfigRequiresDigestAndMPIQueue(t *testing.T) {
	t.Parallel()

	config := testConfig()

	config.RunnerImage = "example.invalid/runner:latest"
	if _, _, _, err := ValidateConfig(config); err == nil || !strings.Contains(err.Error(), "immutable sha256") {
		t.Fatalf("digest validation error = %v", err)
	}

	config = testConfig()

	config.Queue = ""
	if _, _, _, err := ValidateConfig(config); err == nil || !strings.Contains(err.Error(), "queue is required") {
		t.Fatalf("queue validation error = %v", err)
	}
}

func TestValidateConfigRejectsReservedEnvironment(t *testing.T) {
	t.Parallel()

	for _, environment := range []string{
		`{"PATH":"/tmp"}`,
		`{"PROJECT_SIGNAL_RUN_UID":"replacement"}`,
	} {
		config := testConfig()
		config.ScriptEnvironmentJSON = environment

		if _, _, _, err := ValidateConfig(config); err == nil ||
			!strings.Contains(err.Error(), "reserved name") {
			t.Fatalf("environment %s error = %v", environment, err)
		}
	}
}

func TestAggregatePassRequiresCompleteResultsAndIdentities(t *testing.T) {
	t.Parallel()

	resources := testBackendResources()
	results := []Result{
		{ParticipantIndex: 0, ExecutionState: "Succeeded", Verdict: "Pass", Reason: "Healthy"},
		{ParticipantIndex: 1, ExecutionState: "Succeeded", Verdict: "Pass", Reason: "Healthy"},
	}

	output, err := Aggregate(2, resources, results)
	if err != nil {
		t.Fatal(err)
	}

	if output.Verdict != "Pass" || output.Reason != "AllParticipantsPassed" {
		t.Fatalf("output = %#v", output)
	}

	if _, err := Aggregate(2, resources[:1], results); err == nil {
		t.Fatal("Aggregate accepted incomplete backend coverage")
	}

	if _, err := Aggregate(2, resources, results[:1]); err == nil {
		t.Fatal("Aggregate accepted incomplete results")
	}
}

func TestAggregateBackendFailurePrecedesDiagnosticFailure(t *testing.T) {
	t.Parallel()

	output, err := Aggregate(2, testBackendResources(), []Result{
		{ParticipantIndex: 0, ExecutionState: "Succeeded", Verdict: "Fail", Reason: "DiagnosticFailed"},
		{ParticipantIndex: 1, ExecutionState: "Error", Verdict: "Inconclusive", Reason: "BackendFailed"},
	})
	if err != nil {
		t.Fatal(err)
	}

	if output.ExecutionState != "Error" ||
		output.Verdict != "Inconclusive" ||
		output.Reason != "ParticipantCoverageIncomplete" {
		t.Fatalf("output = %#v", output)
	}
}

func TestAggregateRetainsDiagnosticFailureWhenBackendsSucceed(t *testing.T) {
	t.Parallel()

	output, err := Aggregate(2, testBackendResources(), []Result{
		{ParticipantIndex: 0, ExecutionState: "Succeeded", Verdict: "Fail", Reason: "DiagnosticFailed"},
		{ParticipantIndex: 1, ExecutionState: "Succeeded", Verdict: "Pass", Reason: "Healthy"},
	})
	if err != nil {
		t.Fatal(err)
	}

	if output.Verdict != "Fail" || output.Reason != "ParticipantCertificationFailed" {
		t.Fatalf("output = %#v", output)
	}
}

func TestAggregateRejectsDuplicateIdentityAndOversizedMeasurements(t *testing.T) {
	t.Parallel()

	resources := testBackendResources()
	resources[1] = resources[0]
	resources[1].ParticipantIndexes = []int32{1}

	results := []Result{
		{ParticipantIndex: 0, ExecutionState: "Succeeded", Verdict: "Pass", Reason: "Healthy"},
		{ParticipantIndex: 1, ExecutionState: "Succeeded", Verdict: "Pass", Reason: "Healthy"},
	}
	if _, err := Aggregate(2, resources, results); err == nil {
		t.Fatal("Aggregate accepted duplicate backend identity")
	}

	resources = testBackendResources()

	results[1].Measurements = strings.Repeat("x", MaxMeasurementsBytes+1)
	if _, err := Aggregate(2, resources, results); err == nil {
		t.Fatal("Aggregate accepted oversized measurements")
	}
}

func TestParticipantBounds(t *testing.T) {
	t.Parallel()

	if _, err := ParseParticipants("[]", 0, "", ""); err == nil {
		t.Fatal("ParseParticipants accepted zero participants")
	}

	if _, err := ParseParticipants("[]", MaxParticipants+1, "", ""); err == nil {
		t.Fatal("ParseParticipants accepted too many participants")
	}
}

func TestBackendResourceRequiresMatchingParticipantLabel(t *testing.T) {
	t.Parallel()

	resource := ordinaryPod(
		testConfig(),
		testParticipants()[0],
		nil,
		nil,
		signalv1.WorkloadPolicy{},
		testWorkflowOwner(),
		900,
		0,
	)
	resource.UID = types.UID("pod-uid")

	content, err := json.Marshal(resource)
	if err != nil {
		t.Fatal(err)
	}

	var object map[string]any
	if err := json.Unmarshal(content, &object); err != nil {
		t.Fatal(err)
	}

	unstructuredResource := &unstructured.Unstructured{Object: object}
	if _, err := BackendResource(unstructuredResource, 1); err == nil {
		t.Fatal("BackendResource accepted mismatched participant label")
	}
}

func TestParseResultLogUsesLastCompleteRecord(t *testing.T) {
	t.Parallel()

	encode := func(value string) string {
		return base64.StdEncoding.EncodeToString([]byte(value))
	}
	logContent := "noise\n" + resultPrefix +
		encode("Pass") + " " +
		encode("Healthy") + " " +
		encode("temperature=42") + "\n"

	result, err := ParseResultLog(logContent, 1)
	if err != nil {
		t.Fatal(err)
	}

	if result.ParticipantIndex != 1 ||
		result.Verdict != "Pass" ||
		result.Measurements != "temperature=42" {
		t.Fatalf("result = %#v", result)
	}
}

func TestParseResultLogAllowsEmptyMeasurements(t *testing.T) {
	t.Parallel()

	encode := func(value string) string {
		return base64.StdEncoding.EncodeToString([]byte(value))
	}
	logContent := resultPrefix + encode("Pass") + " " + encode("Healthy") + " -\n"

	result, err := ParseResultLog(logContent, 0)
	if err != nil {
		t.Fatal(err)
	}

	if result.Measurements != "" {
		t.Fatalf("measurements = %q", result.Measurements)
	}
}

func testConfig() Config {
	participantsJSON, err := json.Marshal(testParticipants())
	if err != nil {
		panic(err)
	}

	return Config{
		Namespace:             "project-signal",
		WorkflowName:          "run-example-a1",
		RunName:               "run-example",
		RunUID:                types.UID("run-uid"),
		ParticipantsJSON:      string(participantsJSON),
		ParticipantCount:      2,
		RequestNodeName:       "node-a",
		LegacyParticipants:    "node-a,node-b",
		ScriptConfigMap:       "run-example-scripts",
		ScriptKey:             "check.sh",
		ScriptDigest:          strings.Repeat("b", 64),
		RunnerImage:           "example.invalid/runner@sha256:" + strings.Repeat("a", 64),
		BackendAPIVersion:     "kubeflow.org/v2beta1",
		BackendKind:           "MPIJob",
		Queue:                 "certification",
		TolerationsJSON:       `[{"key":"nvidia.com/gpu","operator":"Exists","effect":"NoSchedule"}]`,
		ScriptEnvironmentJSON: `{"CHECK_MODE":"quick"}`,
		GPUResourceCount:      "1",
		Attempt:               1,
		BackoffLimit:          0,
		TTLSeconds:            3600,
	}
}

func testParticipants() []signalv1.Participant {
	return []signalv1.Participant{
		{Index: 0, NodeName: "node-a", NodeUID: types.UID("uid-a")},
		{Index: 1, NodeName: "node-b", NodeUID: types.UID("uid-b")},
	}
}

func testBackendResources() []signalv1.BackendResourceReference {
	return []signalv1.BackendResourceReference{
		{
			APIVersion:         "v1",
			Kind:               "Pod",
			Namespace:          "project-signal",
			Name:               "run-example-p0",
			UID:                types.UID("uid-0"),
			ParticipantIndexes: []int32{0},
		},
		{
			APIVersion:         "v1",
			Kind:               "Pod",
			Namespace:          "project-signal",
			Name:               "run-example-p1",
			UID:                types.UID("uid-1"),
			ParticipantIndexes: []int32{1},
		},
	}
}

func testWorkflowOwner() metav1.OwnerReference {
	controller := true

	return metav1.OwnerReference{
		APIVersion: "argoproj.io/v1alpha1",
		Kind:       "Workflow",
		Name:       "run-example-a1",
		UID:        types.UID("workflow-uid"),
		Controller: &controller,
	}
}

func quantity(value string) *resource.Quantity {
	result := resource.MustParse(value)

	return &result
}
