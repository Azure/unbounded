// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signaladapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	signalv1 "github.com/Azure/unbounded/api/signal/v1alpha1"
)

const testWorkflowServiceAccount = "project-signal-vllm-workload"

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

func TestValidateWorkflowTemplateIdentityUsesApprovedSpecDigest(t *testing.T) {
	t.Parallel()

	template := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{
			"entrypoint": "main",
		},
	}}
	template.SetName("vllm-canary")
	template.SetResourceVersion("42")

	digest, err := workflowTemplateSpecDigest(template)
	if err != nil {
		t.Fatal(err)
	}

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
	}}

	if err := validateWorkflowTemplateIdentity(invocation, template); err != nil {
		t.Fatalf("validateWorkflowTemplateIdentity() error = %v", err)
	}

	if err := unstructured.SetNestedField(template.Object, "changed", "spec", "entrypoint"); err != nil {
		t.Fatal(err)
	}

	if err := validateWorkflowTemplateIdentity(invocation, template); err == nil {
		t.Fatal("validateWorkflowTemplateIdentity() accepted changed template content")
	}
}

func TestWorkflowTemplateRunnerImagesComeFromApprovedTemplate(t *testing.T) {
	t.Parallel()

	image := "example.invalid/vllm@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	template := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "argoproj.io/v1alpha1",
		"kind":       "WorkflowTemplate",
		"metadata": map[string]any{
			"name":      "vllm-canary",
			"namespace": "signal-system",
		},
		"spec": map[string]any{
			"templates": []any{
				map[string]any{
					"name": "main",
					"container": map[string]any{
						"image": image,
					},
				},
			},
		},
	}}

	digest, err := workflowTemplateSpecDigest(template)
	if err != nil {
		t.Fatal(err)
	}

	stage := signalv1.ResolvedStagePlan{
		Test: signalv1.ResolvedCertificationTest{
			Driver: signalv1.CertificationTestDriver{
				Type: signalv1.TestDriverTypeWorkflowTemplate,
				WorkflowTemplate: &signalv1.WorkflowTemplateTestDriver{
					Name:   template.GetName(),
					Digest: digest,
				},
			},
		},
	}
	runner := &Runner{Dynamic: dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), template)}

	images, err := runner.stageRunnerImages(context.Background(), "signal-system", stage)
	if err != nil {
		t.Fatalf("stageRunnerImages() error = %v", err)
	}

	if len(images) != 1 || images[0] != image {
		t.Fatalf("WorkflowTemplate runner images = %#v", images)
	}
}

func TestWorkflowTemplateFailureCleansOwnerFencedChild(t *testing.T) {
	t.Parallel()

	parent := testWorkflow("parent", "parent-uid")
	template := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "argoproj.io/v1alpha1",
		"kind":       "WorkflowTemplate",
		"metadata": map[string]any{
			"name":            "vllm-canary",
			"namespace":       "signal-system",
			"resourceVersion": "42",
		},
		"spec": map[string]any{
			"entrypoint":         "main",
			"serviceAccountName": testWorkflowServiceAccount,
		},
	}}
	dynamicClient := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), parent, template)

	var createdChild *unstructured.Unstructured

	dynamicClient.PrependReactor("create", "workflows", func(action clienttesting.Action) (bool, runtime.Object, error) {
		create := action.(clienttesting.CreateAction)

		child := create.GetObject().(*unstructured.Unstructured).DeepCopy()
		if child.GetName() == parent.GetName() {
			return false, nil, nil
		}

		child.SetUID("child-uid")

		if err := unstructured.SetNestedField(child.Object, "Failed", "status", "phase"); err != nil {
			return true, nil, err
		}

		if err := dynamicClient.Tracker().Create(workflowGVR, child, child.GetNamespace()); err != nil {
			return true, nil, err
		}

		createdChild = child.DeepCopy()

		return true, child, nil
	})

	runner := &Runner{
		Dynamic:      dynamicClient,
		Kubernetes:   kubernetesfake.NewSimpleClientset(),
		PollInterval: time.Millisecond,
	}
	executor := &KubernetesStageExecutor{
		Runner:                 runner,
		Namespace:              "signal-system",
		WorkflowName:           parent.GetName(),
		ExecutionName:          "stage",
		WorkflowServiceAccount: testWorkflowServiceAccount,
	}
	invocation := workflowTemplateInvocation(template)

	_, err := executor.executeWorkflowTemplate(context.Background(), invocation)
	if err == nil {
		t.Fatal("executeWorkflowTemplate() succeeded without a result")
	}

	if createdChild == nil {
		t.Fatal("child Workflow was not created")
	}

	if createdChild.GetLabels()[runUIDLabel] != string(invocation.RunUID) {
		t.Fatalf("child Workflow labels = %#v", createdChild.GetLabels())
	}

	podRunUID, found, nestedErr := unstructured.NestedString(
		createdChild.Object,
		"spec",
		"podMetadata",
		"labels",
		runUIDLabel,
	)
	if nestedErr != nil || !found || podRunUID != string(invocation.RunUID) {
		t.Fatalf(
			"child Workflow Pod Run UID label = %q, found %t, error %v",
			podRunUID,
			found,
			nestedErr,
		)
	}

	arguments, found, nestedErr := unstructured.NestedSlice(
		createdChild.Object,
		"spec",
		"arguments",
		"parameters",
	)
	if nestedErr != nil || !found {
		t.Fatalf("child Workflow arguments found %t, error %v", found, nestedErr)
	}

	var testJSON string

	for _, argument := range arguments {
		parameter := argument.(map[string]any)
		if parameter["name"] == "test-json" {
			testJSON = parameter["value"].(string)
		}
	}

	var test signalv1.ResolvedCertificationTest
	if err := json.Unmarshal([]byte(testJSON), &test); err != nil {
		t.Fatalf("decode child Workflow test-json: %v", err)
	}

	if test.Identity != invocation.Stage.Test.Identity {
		t.Fatalf("child Workflow test identity = %#v, want %#v", test.Identity, invocation.Stage.Test.Identity)
	}

	owners := createdChild.GetOwnerReferences()
	if len(owners) != 1 ||
		owners[0].UID != parent.GetUID() ||
		owners[0].Controller == nil ||
		!*owners[0].Controller ||
		owners[0].BlockOwnerDeletion == nil ||
		!*owners[0].BlockOwnerDeletion {
		t.Fatalf("child Workflow owner fence = %#v", owners)
	}

	_, getErr := dynamicClient.Resource(workflowGVR).Namespace("signal-system").
		Get(context.Background(), createdChild.GetName(), metav1.GetOptions{})
	if !apierrors.IsNotFound(getErr) {
		t.Fatalf("child Workflow remains after failure: %v", getErr)
	}
}

func TestWorkflowTemplateCancellationCleansChild(t *testing.T) {
	t.Parallel()

	parent := testWorkflow("parent", "parent-uid")
	template := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "argoproj.io/v1alpha1",
		"kind":       "WorkflowTemplate",
		"metadata": map[string]any{
			"name":            "vllm-canary",
			"namespace":       "signal-system",
			"resourceVersion": "42",
		},
		"spec": map[string]any{
			"entrypoint":         "main",
			"serviceAccountName": testWorkflowServiceAccount,
		},
	}}
	dynamicClient := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), parent, template)
	runner := &Runner{
		Dynamic:      dynamicClient,
		Kubernetes:   kubernetesfake.NewSimpleClientset(),
		PollInterval: time.Millisecond,
	}
	executor := &KubernetesStageExecutor{
		Runner:                 runner,
		Namespace:              "signal-system",
		WorkflowName:           parent.GetName(),
		ExecutionName:          "stage",
		WorkflowServiceAccount: testWorkflowServiceAccount,
	}
	invocation := workflowTemplateInvocation(template)
	childName := invocationResourceName(executor.ExecutionName, invocation, -1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := executor.executeWorkflowTemplate(ctx, invocation)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("executeWorkflowTemplate() error = %v, want context cancellation", err)
	}

	_, getErr := dynamicClient.Resource(workflowGVR).Namespace("signal-system").
		Get(context.Background(), childName, metav1.GetOptions{})
	if !apierrors.IsNotFound(getErr) {
		t.Fatalf("child Workflow remains after cancellation: %v", getErr)
	}
}

func TestWorkflowTemplateCleanupDeletesLateResult(t *testing.T) {
	t.Parallel()

	workflow := testWorkflow("child", "child-uid")
	dynamicClient := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), workflow)
	kubernetesClient := kubernetesfake.NewSimpleClientset()
	resultName := "stage-result"
	resultGets := 0

	kubernetesClient.PrependReactor("get", "configmaps", func(action clienttesting.Action) (bool, runtime.Object, error) {
		resultGets++
		if resultGets == 1 {
			return true, nil, apierrors.NewNotFound(schema.GroupResource{Resource: "configmaps"}, resultName)
		}

		if resultGets == 2 {
			configMap := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resultName,
					Namespace: "signal-system",
					UID:       "result-uid",
				},
			}
			if err := kubernetesClient.Tracker().Create(
				corev1.SchemeGroupVersion.WithResource("configmaps"),
				configMap,
				configMap.Namespace,
			); err != nil {
				return true, nil, err
			}
		}

		return false, nil, nil
	})

	executor := &KubernetesStageExecutor{
		Runner: &Runner{
			Dynamic:      dynamicClient,
			Kubernetes:   kubernetesClient,
			PollInterval: time.Millisecond,
		},
		Namespace: "signal-system",
	}

	if err := executor.cleanupWorkflowInvocation(context.Background(), workflow, resultName); err != nil {
		t.Fatalf("cleanupWorkflowInvocation() error = %v", err)
	}

	_, err := kubernetesClient.CoreV1().ConfigMaps("signal-system").
		Get(context.Background(), resultName, metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("late result ConfigMap remains after cleanup: %v", err)
	}
}

func TestStagePodObservationFailureCleansPod(t *testing.T) {
	t.Parallel()

	kubernetesClient := kubernetesfake.NewSimpleClientset()
	podGets := 0
	podCreates := 0
	allPodsCreatedBeforeWait := false

	kubernetesClient.PrependReactor("create", "pods", func(clienttesting.Action) (bool, runtime.Object, error) {
		podCreates++

		return false, nil, nil
	})

	kubernetesClient.PrependReactor("get", "pods", func(clienttesting.Action) (bool, runtime.Object, error) {
		podGets++
		if podGets == 1 {
			allPodsCreatedBeforeWait = podCreates == 2

			return true, nil, errors.New("observe failed")
		}

		return false, nil, nil
	})

	executor := &KubernetesStageExecutor{
		Runner: &Runner{
			Dynamic:      dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), testWorkflow("parent", "parent-uid")),
			Kubernetes:   kubernetesClient,
			PollInterval: time.Millisecond,
		},
		Namespace:     "signal-system",
		WorkflowName:  "parent",
		ExecutionName: "stage",
	}

	invocation := stageExecutionInvocation()
	invocation.Participants = []signalv1.Participant{
		{Index: 0, NodeName: "worker-0", NodeUID: "node-uid-0"},
		{Index: 1, NodeName: "worker-1", NodeUID: "node-uid-1"},
	}
	invocation.ParticipantRole = map[int32]string{0: "server", 1: "client"}

	_, err := executor.executePods(
		context.Background(),
		invocation,
	)
	if err == nil || !strings.Contains(err.Error(), "observe stage Pod") {
		t.Fatalf("executePods() error = %v, want observation failure", err)
	}

	if !allPodsCreatedBeforeWait {
		t.Fatalf("created Pods before first wait = %d, want 2", podCreates)
	}

	pods, err := kubernetesClient.CoreV1().Pods("signal-system").
		List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list stage Pods: %v", err)
	}

	if len(pods.Items) != 0 {
		t.Fatalf("stage Pods remain after observation failure: %#v", pods.Items)
	}
}

func TestFailedStagePodCannotReportPassedInvocation(t *testing.T) {
	t.Parallel()

	invocation := stageExecutionInvocation()
	envelope := signalv1.InvocationResultEnvelope{
		Outcome: signalv1.InvocationOutcomePassed,
	}

	content, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "stage-pod",
			Namespace: "signal-system",
			UID:       "pod-uid",
		},
		Status: corev1.PodStatus{Phase: corev1.PodFailed},
	}
	executor := &KubernetesStageExecutor{
		Runner: &Runner{
			Kubernetes: kubernetesfake.NewSimpleClientset(pod),
			ReadPodLog: func(context.Context, string, string, string, bool) (string, error) {
				return invocationResultPrefix + string(content), nil
			},
			PollInterval: time.Millisecond,
		},
		Namespace: "signal-system",
	}

	_, err = executor.waitStagePod(context.Background(), invocation, pod)
	if err == nil || !strings.Contains(err.Error(), "failed stage Pod reported a passed invocation") {
		t.Fatalf("waitStagePod() error = %v", err)
	}
}

func TestInvocationOutcomePrecedencePreservesNonPassSeverity(t *testing.T) {
	t.Parallel()

	if invocationOutcomePrecedence(signalv1.InvocationOutcomeFailed) <=
		invocationOutcomePrecedence(signalv1.InvocationOutcomeInconclusive) {
		t.Fatal("Failed outcome does not take precedence over Inconclusive")
	}

	if invocationOutcomePrecedence(signalv1.InvocationOutcomeCanceled) <=
		invocationOutcomePrecedence(signalv1.InvocationOutcomeFailed) {
		t.Fatal("Canceled outcome does not take precedence over Failed")
	}
}

func TestStagePodHasRunUIDLabelAndOwnerFence(t *testing.T) {
	t.Parallel()

	parent := testWorkflow("parent", "parent-uid")
	executor := &KubernetesStageExecutor{
		Namespace:     "signal-system",
		ExecutionName: "stage",
	}
	invocation := stageExecutionInvocation()

	pod, err := executor.stagePod(parent, invocation, 0)
	if err != nil {
		t.Fatalf("stagePod() error = %v", err)
	}

	if pod.Labels[runUIDLabel] != string(invocation.RunUID) {
		t.Fatalf("stage Pod labels = %#v", pod.Labels)
	}

	owners := pod.OwnerReferences
	if len(owners) != 1 ||
		owners[0].UID != parent.GetUID() ||
		owners[0].BlockOwnerDeletion == nil ||
		!*owners[0].BlockOwnerDeletion {
		t.Fatalf("stage Pod owner fence = %#v", owners)
	}

	if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
		t.Fatal("stage Pod must not automount a ServiceAccount token")
	}
}

func TestWorkflowTemplateRejectsUnauthorizedServiceAccountSelection(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{
			name: "privileged top-level ServiceAccount",
			mutate: func(spec map[string]any) {
				spec["serviceAccountName"] = "project-signal-controller"
			},
		},
		{
			name: "privileged template ServiceAccount",
			mutate: func(spec map[string]any) {
				spec["templates"] = []any{
					map[string]any{
						"name":               "main",
						"serviceAccountName": "project-signal-adapter",
					},
				}
			},
		},
		{
			name: "PodSpec patch",
			mutate: func(spec map[string]any) {
				spec["podSpecPatch"] = `{"serviceAccountName":"project-signal-controller"}`
			},
		},
		{
			name: "referenced WorkflowTemplate",
			mutate: func(spec map[string]any) {
				spec["templates"] = []any{
					map[string]any{
						"name": "main",
						"templateRef": map[string]any{
							"name":     "privileged-template",
							"template": "main",
						},
					},
				}
			},
		},
		{
			name: "referenced WorkflowTemplate spec",
			mutate: func(spec map[string]any) {
				spec["workflowTemplateRef"] = map[string]any{
					"name": "privileged-template",
				}
			},
		},
		{
			name: "reserved Run UID template label",
			mutate: func(spec map[string]any) {
				spec["templates"] = []any{
					map[string]any{
						"name": "main",
						"metadata": map[string]any{
							"labels": map[string]any{
								runUIDLabel: "different-run-uid",
							},
						},
					},
				}
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			spec := map[string]any{
				"entrypoint":         "main",
				"serviceAccountName": testWorkflowServiceAccount,
			}
			tt.mutate(spec)

			if err := validateWorkflowServiceAccount(spec, testWorkflowServiceAccount); err == nil {
				t.Fatal("validateWorkflowServiceAccount() accepted unauthorized ServiceAccount selection")
			}
		})
	}
}

func TestStagePodAppliesHostPathRequirements(t *testing.T) {
	t.Parallel()

	invocation := stageExecutionInvocation()
	invocation.Stage.Test.Security.HostPaths = []signalv1.WorkloadHostPath{{
		Path:      "/sys",
		MountPath: "/host/sys",
		ReadOnly:  true,
	}}
	executor := &KubernetesStageExecutor{
		Namespace:     "signal-system",
		ExecutionName: "stage",
	}

	pod, err := executor.stagePod(testWorkflow("parent", "parent-uid"), invocation, 0)
	if err != nil {
		t.Fatalf("stagePod() error = %v", err)
	}

	if len(pod.Spec.Volumes) != 1 ||
		pod.Spec.Volumes[0].HostPath == nil ||
		pod.Spec.Volumes[0].HostPath.Path != "/sys" {
		t.Fatalf("stage Pod host path volumes = %#v", pod.Spec.Volumes)
	}

	mounts := pod.Spec.Containers[0].VolumeMounts
	if len(mounts) != 1 ||
		mounts[0].MountPath != "/host/sys" ||
		!mounts[0].ReadOnly {
		t.Fatalf("stage Pod host path mounts = %#v", mounts)
	}
}

func TestStagePodUsesFileHostPathTypeForApprovedProcLeaf(t *testing.T) {
	t.Parallel()

	invocation := stageExecutionInvocation()
	invocation.Stage.Test.Security.HostPaths = []signalv1.WorkloadHostPath{{
		Path:      "/proc/meminfo",
		MountPath: "/host/meminfo",
		ReadOnly:  true,
	}}
	executor := &KubernetesStageExecutor{
		Namespace:     "signal-system",
		ExecutionName: "stage",
	}

	pod, err := executor.stagePod(testWorkflow("parent", "parent-uid"), invocation, 0)
	if err != nil {
		t.Fatalf("stagePod() error = %v", err)
	}

	if pod.Spec.Volumes[0].HostPath == nil ||
		pod.Spec.Volumes[0].HostPath.Type == nil ||
		*pod.Spec.Volumes[0].HostPath.Type != corev1.HostPathFile {
		t.Fatalf("proc host path volume = %#v", pod.Spec.Volumes[0])
	}
}

func TestLegacyHostPathMountPreservesWritablePolicy(t *testing.T) {
	t.Parallel()

	policy := signalv1.WorkloadPolicy{HostPaths: []signalv1.WorkloadHostPath{{
		Path:      "/var/lib/project-signal/output",
		MountPath: "/work",
		ReadOnly:  false,
	}}}

	volumes := workloadVolumes("scripts", policy)
	if volumes[len(volumes)-1].HostPath == nil ||
		volumes[len(volumes)-1].HostPath.Type == nil ||
		*volumes[len(volumes)-1].HostPath.Type != corev1.HostPathDirectory {
		t.Fatalf("legacy host path volume = %#v", volumes[len(volumes)-1])
	}

	mounts := workloadVolumeMounts(policy)
	if len(mounts) != 1 || mounts[0].ReadOnly {
		t.Fatalf("legacy host path mounts = %#v", mounts)
	}
}

func TestCancelInvocationUsesGlobalParticipantIndex(t *testing.T) {
	t.Parallel()

	kubernetesClient := kubernetesfake.NewSimpleClientset()
	deletedPod := ""

	kubernetesClient.PrependReactor("delete", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
		deletedPod = action.(clienttesting.DeleteAction).GetName()

		return true, nil, nil
	})
	executor := &KubernetesStageExecutor{
		Runner: &Runner{
			Dynamic:    dynamicfake.NewSimpleDynamicClient(runtime.NewScheme()),
			Kubernetes: kubernetesClient,
		},
		Namespace:     "signal-system",
		ExecutionName: "stage",
	}
	invocation := stageExecutionInvocation()
	invocation.Participants[0].Index = 2
	invocation.ParticipantRole = map[int32]string{2: "worker"}

	if err := executor.CancelInvocation(context.Background(), invocation); err != nil {
		t.Fatalf("CancelInvocation() error = %v", err)
	}

	want := invocationResourceName(executor.ExecutionName, invocation, 2)
	if deletedPod != want {
		t.Fatalf("deleted Pod = %q, want %q", deletedPod, want)
	}
}

func TestInvocationResourceNamePreservesUniqueSuffix(t *testing.T) {
	t.Parallel()

	invocation := stageExecutionInvocation()
	base := strings.Repeat("a", 63)

	first := invocationResourceName(base, invocation, 0)
	second := invocationResourceName(base, invocation, 1)
	workflow := invocationResourceName(base, invocation, -1)

	if first == second || first == workflow || second == workflow {
		t.Fatalf("invocation resource names collide: %q, %q, %q", first, second, workflow)
	}

	if len(first) > 63 || !strings.HasSuffix(first, "-p0") ||
		len(second) > 63 || !strings.HasSuffix(second, "-p1") {
		t.Fatalf("invocation resource suffixes were truncated: %q, %q", first, second)
	}
}

func TestScriptInvocationWrapperUsesApprovedLanguage(t *testing.T) {
	t.Parallel()

	shell := scriptInvocationWrapper(signalv1.ScriptLanguageShell, "check.sh", testStageDigest("script"))
	if !strings.Contains(shell, `exec /bin/sh "$script"`) {
		t.Fatalf("shell wrapper = %q", shell)
	}

	python := scriptInvocationWrapper(signalv1.ScriptLanguagePython, "check.py", testStageDigest("script"))
	if !strings.Contains(python, `exec python3 "$script"`) {
		t.Fatalf("python wrapper = %q", python)
	}
}

func TestMergeWorkflowArgumentsPreservesApprovedDefaultsAndArtifacts(t *testing.T) {
	t.Parallel()

	existing := map[string]any{
		"parameters": []any{
			map[string]any{"name": "model", "value": "approved-model"},
			map[string]any{"name": "run-uid", "value": "default"},
		},
		"artifacts": []any{
			map[string]any{"name": "dataset", "from": "approved"},
		},
	}

	merged, err := mergeWorkflowArguments(existing, []any{
		map[string]any{"name": "run-uid", "value": "immutable-run-uid"},
		map[string]any{"name": "plan-digest", "value": testStageDigest("plan")},
	})
	if err != nil {
		t.Fatalf("mergeWorkflowArguments() error = %v", err)
	}

	if len(merged["artifacts"].([]any)) != 1 {
		t.Fatalf("merged artifacts = %#v", merged["artifacts"])
	}

	parameters := merged["parameters"].([]any)

	values := make(map[string]any, len(parameters))
	for _, parameter := range parameters {
		typed := parameter.(map[string]any)
		values[typed["name"].(string)] = typed["value"]
	}

	if values["model"] != "approved-model" ||
		values["run-uid"] != "immutable-run-uid" ||
		values["plan-digest"] != testStageDigest("plan") {
		t.Fatalf("merged parameters = %#v", values)
	}
}

func TestMergeWorkflowArgumentsBoundsAllocation(t *testing.T) {
	t.Parallel()

	adapterParameters := []any{
		map[string]any{"name": "run-uid", "value": "immutable-run-uid"},
		map[string]any{"name": "plan-digest", "value": testStageDigest("plan")},
	}

	existingParameters := make([]any, maximumWorkflowArgumentParameters-len(adapterParameters))
	for index := range existingParameters {
		existingParameters[index] = map[string]any{
			"name":  fmt.Sprintf("approved-%03d", index),
			"value": "value",
		}
	}

	merged, err := mergeWorkflowArguments(
		map[string]any{"parameters": existingParameters},
		adapterParameters,
	)
	if err != nil {
		t.Fatalf("mergeWorkflowArguments() boundary error = %v", err)
	}

	if got := len(merged["parameters"].([]any)); got != maximumWorkflowArgumentParameters {
		t.Fatalf("merged parameter count = %d, want %d", got, maximumWorkflowArgumentParameters)
	}

	overLimit := append(existingParameters, map[string]any{"name": "overflow", "value": "value"})
	if _, err := mergeWorkflowArguments(
		map[string]any{"parameters": overLimit},
		adapterParameters,
	); err == nil {
		t.Fatal("mergeWorkflowArguments() accepted an over-limit allocation")
	}

	tooManyInjected := make([]any, maximumWorkflowArgumentParameters+1)
	if _, err := mergeWorkflowArguments(nil, tooManyInjected); err == nil {
		t.Fatal("mergeWorkflowArguments() accepted an overflowing injected parameter count")
	}
}

func TestLegacyRunLabelsIncludeExactRunUID(t *testing.T) {
	t.Parallel()

	labels := runLabels(Config{
		ExecutionName: "execution",
		RunUID:        "run-uid",
	})
	if labels[runUIDLabel] != "run-uid" {
		t.Fatalf("legacy workload labels = %#v", labels)
	}
}

func testWorkflow(name string, uid types.UID) *unstructured.Unstructured {
	workflow := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "argoproj.io/v1alpha1",
		"kind":       "Workflow",
		"metadata": map[string]any{
			"name":      name,
			"namespace": "signal-system",
		},
	}}
	workflow.SetUID(uid)

	return workflow
}

func workflowTemplateInvocation(template *unstructured.Unstructured) StageInvocation {
	invocation := stageExecutionInvocation()

	digest, err := workflowTemplateSpecDigest(template)
	if err != nil {
		panic(err)
	}

	invocation.Stage.Test.Driver = signalv1.CertificationTestDriver{
		Type: signalv1.TestDriverTypeWorkflowTemplate,
		WorkflowTemplate: &signalv1.WorkflowTemplateTestDriver{
			Name:   "vllm-canary",
			Digest: digest,
		},
	}

	return invocation
}

func stageExecutionInvocation() StageInvocation {
	return StageInvocation{
		ID:         "inv-00000000000000000000000000000000",
		RunUID:     "run-uid",
		PlanDigest: testStageDigest("plan"),
		StageOrder: 0,
		Stage: signalv1.ResolvedStagePlan{
			Name: "stage",
			Test: signalv1.ResolvedCertificationTest{
				Identity: signalv1.TestIdentitySnapshot{
					Name:          "test",
					UID:           "test-uid",
					Generation:    1,
					ContentDigest: testStageDigest("test"),
					Trust:         signalv1.CertificationTestTrustApproved,
				},
				Driver: signalv1.CertificationTestDriver{
					Type: signalv1.TestDriverTypeContainer,
					Container: &signalv1.ContainerTestDriver{
						Image: "example.invalid/test@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
					},
				},
			},
			Schedule: signalv1.StageSchedule{
				Mode:              signalv1.CheckExecutionModeRunOnce,
				InvocationTimeout: metav1.Duration{Duration: time.Minute},
			},
		},
		Participants: []signalv1.Participant{{
			Index:    0,
			NodeName: "worker-0",
			NodeUID:  "node-uid",
		}},
		ParticipantRole: map[int32]string{0: "participant"},
	}
}

func TestStagePodCarriesScriptResourcesAndImmutableIdentity(t *testing.T) {
	t.Parallel()

	workflow := &unstructured.Unstructured{}
	workflow.SetAPIVersion("argoproj.io/v1alpha1")
	workflow.SetKind("Workflow")
	workflow.SetName("run-workflow")
	workflow.SetUID("workflow-uid")

	invocation := StageInvocation{
		ID:         "invocation-1",
		RunUID:     types.UID("run-uid"),
		PlanDigest: testStageDigest("plan"),
		StageOrder: 2,
		Stage: signalv1.ResolvedStagePlan{
			Name: "gpu-inventory",
			Test: signalv1.ResolvedCertificationTest{
				Identity: signalv1.TestIdentitySnapshot{
					Name:          "gpu-inventory",
					UID:           types.UID("test-uid"),
					Generation:    3,
					ContentDigest: testStageDigest("test"),
					Trust:         signalv1.CertificationTestTrustApproved,
				},
				Driver: signalv1.CertificationTestDriver{
					Type: signalv1.TestDriverTypeScript,
					Script: &signalv1.ScriptTestDriver{
						Language: signalv1.ScriptLanguageShell,
						Source: signalv1.ImmutableArtifactSource{
							Type: signalv1.ArtifactSourceTypeConfigMap,
							ConfigMap: &signalv1.ConfigMapArtifactSource{
								Name:   "gpu-inventory",
								Key:    "gpu-inventory.sh",
								Digest: testStageDigest("script"),
							},
						},
						RunnerImage: "example.invalid/runner@sha256:" + strings.Repeat("a", 64),
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceName("nvidia.com/gpu"): resource.MustParse("8"),
							},
						},
					},
				},
			},
			Schedule: signalv1.StageSchedule{
				Mode:              signalv1.CheckExecutionModeRunOnce,
				InvocationTimeout: metav1.Duration{Duration: 2},
			},
		},
		Participants: []signalv1.Participant{{
			NodeName: "participant",
			NodeUID:  types.UID("participant-uid"),
		}},
		ParticipantRole: map[int32]string{0: "participant"},
	}

	executor := KubernetesStageExecutor{Namespace: "signal-system", ExecutionName: "execution"}

	pod, err := executor.stagePod(workflow, invocation, 0)
	if err != nil {
		t.Fatalf("stagePod() error = %v", err)
	}

	gpu := pod.Spec.Containers[0].Resources.Requests[corev1.ResourceName("nvidia.com/gpu")]
	if gpu.Cmp(resource.MustParse("8")) != 0 {
		t.Fatalf("script GPU request = %s, want 8", gpu.String())
	}

	env := make(map[string]string)
	for _, variable := range pod.Spec.Containers[0].Env {
		env[variable.Name] = variable.Value
	}

	for name, want := range map[string]string{
		"PROJECT_SIGNAL_TEST_NAME":           "gpu-inventory",
		"PROJECT_SIGNAL_TEST_UID":            "test-uid",
		"PROJECT_SIGNAL_TEST_GENERATION":     "3",
		"PROJECT_SIGNAL_TEST_CONTENT_DIGEST": testStageDigest("test"),
		"PROJECT_SIGNAL_TEST_TRUST":          string(signalv1.CertificationTestTrustApproved),
	} {
		if env[name] != want {
			t.Fatalf("%s = %q, want %q", name, env[name], want)
		}
	}
}
