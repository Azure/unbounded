// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signaladapter

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	clienttesting "k8s.io/client-go/testing"
	utilptr "k8s.io/utils/ptr"

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

func TestWorkflowTemplatePersistentVolumeClaimRequiresExclusiveRelease(t *testing.T) {
	t.Parallel()

	const (
		namespace = "signal-system"
		claimName = "approved-model-cache"
		pvName    = "approved-model-cache-pv"
	)

	templateSpec := map[string]any{
		"templates": []any{map[string]any{
			"name": "main",
			"volumes": []any{map[string]any{
				"name": "model-cache",
				"persistentVolumeClaim": map[string]any{
					"claimName": claimName,
				},
			}},
		}},
	}
	claim := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: claimName, Namespace: namespace},
		Spec:       corev1.PersistentVolumeClaimSpec{VolumeName: pvName},
		Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}

	t.Run("ready after release", func(t *testing.T) {
		t.Parallel()

		executor := &KubernetesStageExecutor{
			Namespace: namespace,
			Runner: &Runner{
				Kubernetes: kubernetesfake.NewSimpleClientset(claim.DeepCopy()),
			},
		}

		if err := executor.validateWorkflowPersistentVolumeClaimsReady(
			context.Background(),
			workflowPersistentVolumeClaims(templateSpec),
		); err != nil {
			t.Fatalf("validateWorkflowPersistentVolumeClaimsReady() error = %v", err)
		}
	})

	t.Run("seed Pod remains", func(t *testing.T) {
		t.Parallel()

		seedPod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "cache-seed", Namespace: namespace},
			Spec: corev1.PodSpec{Volumes: []corev1.Volume{{
				Name: "model-cache",
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
						ClaimName: claimName,
					},
				},
			}}},
		}
		executor := &KubernetesStageExecutor{
			Namespace: namespace,
			Runner: &Runner{
				Kubernetes: kubernetesfake.NewSimpleClientset(claim.DeepCopy(), seedPod),
			},
		}

		err := executor.validateWorkflowPersistentVolumeClaimsReady(
			context.Background(),
			workflowPersistentVolumeClaims(templateSpec),
		)
		if err == nil || !strings.Contains(err.Error(), "still referenced by Pod") {
			t.Fatalf("validateWorkflowPersistentVolumeClaimsReady() error = %v, want Pod reference", err)
		}
	})

	t.Run("VolumeAttachment remains", func(t *testing.T) {
		t.Parallel()

		attachment := &storagev1.VolumeAttachment{
			ObjectMeta: metav1.ObjectMeta{Name: "cache-attachment"},
			Spec: storagev1.VolumeAttachmentSpec{
				Attacher: "example.csi.invalid",
				NodeName: "gpu-node",
				Source: storagev1.VolumeAttachmentSource{
					PersistentVolumeName: utilptr.To(pvName),
				},
			},
		}
		executor := &KubernetesStageExecutor{
			Namespace: namespace,
			Runner: &Runner{
				Kubernetes: kubernetesfake.NewSimpleClientset(claim.DeepCopy(), attachment),
			},
		}

		err := executor.validateWorkflowPersistentVolumeClaimsReady(
			context.Background(),
			workflowPersistentVolumeClaims(templateSpec),
		)
		if err == nil || !strings.Contains(err.Error(), "still referenced by VolumeAttachment") {
			t.Fatalf(
				"validateWorkflowPersistentVolumeClaimsReady() error = %v, want VolumeAttachment reference",
				err,
			)
		}
	})
}

func TestWorkflowTemplatePersistentVolumeClaimLockIsExclusiveAndUIDFenced(t *testing.T) {
	t.Parallel()

	const namespace = "signal-system"

	parent := testWorkflow("parent", "parent-uid")
	invocation := StageInvocation{ID: "invocation", RunUID: "run-uid"}
	kubernetesClient := kubernetesfake.NewSimpleClientset()
	executor := &KubernetesStageExecutor{
		Namespace: namespace,
		Runner:    &Runner{Kubernetes: kubernetesClient},
	}

	locks, err := executor.acquireWorkflowPersistentVolumeClaimLocks(
		context.Background(),
		[]string{"approved-model-cache"},
		parent,
		invocation,
	)
	if err != nil {
		t.Fatalf("acquireWorkflowPersistentVolumeClaimLocks() error = %v", err)
	}

	if len(locks) != 1 {
		t.Fatalf("acquired locks = %d, want 1", len(locks))
	}

	_, err = executor.acquireWorkflowPersistentVolumeClaimLocks(
		context.Background(),
		[]string{"approved-model-cache"},
		parent,
		invocation,
	)
	if err == nil || !apierrors.IsAlreadyExists(err) {
		t.Fatalf("second lock acquisition error = %v, want AlreadyExists", err)
	}

	locks[0].UID = "lock-uid"
	lockUID := locks[0].UID

	kubernetesClient.PrependReactor("delete", "configmaps", func(action clienttesting.Action) (
		bool,
		runtime.Object,
		error,
	) {
		deletion := action.(clienttesting.DeleteAction).GetDeleteOptions()
		if deletion.Preconditions == nil ||
			deletion.Preconditions.UID == nil ||
			*deletion.Preconditions.UID != lockUID {
			t.Fatalf("lock deletion preconditions = %#v, want UID %q", deletion.Preconditions, lockUID)
		}

		return false, nil, nil
	})

	if err := executor.releaseWorkflowPersistentVolumeClaimLocks(
		context.Background(),
		locks,
	); err != nil {
		t.Fatalf("releaseWorkflowPersistentVolumeClaimLocks() error = %v", err)
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

	podWorkflow, found, nestedErr := unstructured.NestedString(
		createdChild.Object,
		"spec",
		"podMetadata",
		"labels",
		argoWorkflowLabel,
	)
	if nestedErr != nil || !found || podWorkflow != createdChild.GetName() {
		t.Fatalf(
			"child Workflow Pod owner label = %q, found %t, error %v",
			podWorkflow,
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

func TestValidateWorkflowRestartObservationSpec(t *testing.T) {
	t.Parallel()

	valid := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{
			"entrypoint": "canary",
			"templates": []any{map[string]any{
				"name": "canary",
				"retryStrategy": map[string]any{
					"limit": "0",
				},
				"container": map[string]any{
					"image": "example.invalid/canary@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				},
			}},
		},
	}}

	if err := validateWorkflowRestartObservationSpec(valid); err != nil {
		t.Fatalf("validateWorkflowRestartObservationSpec() error = %v", err)
	}

	for _, testCase := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{
			name: "pod gc",
			mutate: func(spec map[string]any) {
				spec["podGC"] = map[string]any{"strategy": "OnWorkflowCompletion"}
			},
		},
		{
			name: "ttl strategy",
			mutate: func(spec map[string]any) {
				spec["ttlStrategy"] = map[string]any{"secondsAfterCompletion": int64(30)}
			},
		},
		{
			name: "multiple Pod templates",
			mutate: func(spec map[string]any) {
				spec["templates"] = append(
					spec["templates"].([]any),
					map[string]any{
						"name":          "second",
						"retryStrategy": map[string]any{"limit": "0"},
						"container":     map[string]any{"image": "example.invalid/second"},
					},
				)
			},
		},
		{
			name: "retry enabled",
			mutate: func(spec map[string]any) {
				template := spec["templates"].([]any)[0].(map[string]any)
				template["retryStrategy"] = map[string]any{"limit": "1"}
			},
		},
		{
			name: "steps template",
			mutate: func(spec map[string]any) {
				template := spec["templates"].([]any)[0].(map[string]any)
				template["steps"] = []any{}
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			workflow := valid.DeepCopy()
			spec := workflow.Object["spec"].(map[string]any)
			testCase.mutate(spec)

			if err := validateWorkflowRestartObservationSpec(workflow); err == nil {
				t.Fatal("validateWorkflowRestartObservationSpec() accepted ambiguous Workflow shape")
			}
		})
	}
}

func TestObserveWorkflowRestartSources(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name             string
		pods             []*corev1.Pod
		wantContainer    int32
		wantWorkflowPods int32
		wantError        string
	}{
		{
			name:             "zero observed restarts",
			pods:             []*corev1.Pod{workflowChildPod("pod-0", "child-uid", 0)},
			wantContainer:    0,
			wantWorkflowPods: 0,
		},
		{
			name:             "container restart observed",
			pods:             []*corev1.Pod{workflowChildPod("pod-0", "child-uid", 2)},
			wantContainer:    2,
			wantWorkflowPods: 0,
		},
		{
			name: "additional Workflow Pod observed",
			pods: []*corev1.Pod{
				workflowChildPod("pod-0", "child-uid", 0),
				workflowChildPod("pod-1", "child-uid", 0),
			},
			wantContainer:    0,
			wantWorkflowPods: 1,
		},
		{
			name:      "missing Pod status",
			wantError: "no observable Pod",
		},
		{
			name: "ambiguous Pod ownership",
			pods: []*corev1.Pod{
				workflowChildPod("pod-0", "different-child-uid", 0),
			},
			wantError: "ownership is ambiguous",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			parent := testWorkflow("parent", "parent-uid")
			child := ownedWorkflow("child", "child-uid", parent, "run-uid")

			objects := make([]runtime.Object, len(tt.pods))
			for index := range tt.pods {
				objects[index] = tt.pods[index]
			}

			executor := &KubernetesStageExecutor{
				Runner: &Runner{
					Dynamic:    dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), parent, child),
					Kubernetes: kubernetesfake.NewSimpleClientset(objects...),
				},
				Namespace: "signal-system",
			}
			invocation := stageExecutionInvocation()

			counts, err := executor.observeWorkflowRestartSources(
				context.Background(),
				invocation,
				parent,
				child,
			)
			if tt.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantError) {
					t.Fatalf("observeWorkflowRestartSources() error = %v, want %q", err, tt.wantError)
				}

				return
			}

			if err != nil {
				t.Fatalf("observeWorkflowRestartSources() error = %v", err)
			}

			if counts.Container != tt.wantContainer || counts.WorkflowPod != tt.wantWorkflowPods {
				t.Fatalf("restart counts = %#v", counts)
			}
		})
	}
}

func TestWorkflowRestartMetricUsesPlatformObservationAndPersists(t *testing.T) {
	t.Parallel()

	forged := `{"container":0,"workflowPod":0}`
	envelope := signalv1.InvocationResultEnvelope{
		Measurements: []signalv1.MetricMeasurement{{
			Name: workflowRestartMetricName,
			Value: signalv1.TypedValue{
				Type:        signalv1.TypedValueTypeString,
				StringValue: &forged,
			},
		}},
	}
	rawContent := []byte(`{"forged":"raw-result"}`)

	if err := replaceWorkflowRestartMetric(
		&envelope,
		workflowRestartSourceCounts{Container: 2, WorkflowPod: 1},
	); err != nil {
		t.Fatalf("replaceWorkflowRestartMetric() error = %v", err)
	}

	if err := appendRawWorkflowResultArtifact(&envelope, "result-name", rawContent); err != nil {
		t.Fatalf("appendRawWorkflowResultArtifact() error = %v", err)
	}

	value := envelope.Measurements[0].Value.StringValue
	if value == nil || *value != `{"container":2,"workflowPod":1}` {
		t.Fatalf("platform restart metric = %v", value)
	}

	if len(envelope.Artifacts) != 1 ||
		envelope.Artifacts[0].Name != rawWorkflowResultArtifactName ||
		envelope.Artifacts[0].Digest != testStageDigest(string(rawContent)) {
		t.Fatalf("raw result provenance = %#v", envelope.Artifacts)
	}

	persisted, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}

	var resumed signalv1.InvocationResultEnvelope
	if err := json.Unmarshal(persisted, &resumed); err != nil {
		t.Fatal(err)
	}

	beforeDigest, err := invocationSequenceDigest([]signalv1.InvocationResultEnvelope{envelope})
	if err != nil {
		t.Fatal(err)
	}

	afterDigest, err := invocationSequenceDigest([]signalv1.InvocationResultEnvelope{resumed})
	if err != nil {
		t.Fatal(err)
	}

	resumedValue := resumed.Measurements[0].Value.StringValue
	if resumedValue == nil || *resumedValue != *value ||
		resumed.Artifacts[0].Digest != envelope.Artifacts[0].Digest ||
		beforeDigest != afterDigest {
		t.Fatalf("resumed envelope lost platform observation: %#v", resumed)
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

func TestWaitStagePodUsesKubernetesLogsWithoutHook(t *testing.T) {
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
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Pod",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "stage-pod",
			Namespace: "signal-system",
			UID:       "pod-uid",
		},
		Status: corev1.PodStatus{Phase: corev1.PodFailed},
	}

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if strings.HasSuffix(request.URL.Path, "/log") {
			_, _ = writer.Write([]byte(invocationResultPrefix + string(content)))

			return
		}

		writer.Header().Set("Content-Type", "application/json")

		if err := json.NewEncoder(writer).Encode(pod); err != nil {
			t.Errorf("encode Pod response: %v", err)
		}
	}))
	defer server.Close()

	client, err := kubernetes.NewForConfig(&rest.Config{Host: server.URL})
	if err != nil {
		t.Fatal(err)
	}

	executor := &KubernetesStageExecutor{
		Runner: &Runner{
			Kubernetes:   client,
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
		{
			name: "reserved Workflow template label",
			mutate: func(spec map[string]any) {
				spec["podMetadata"] = map[string]any{
					"labels": map[string]any{
						argoWorkflowLabel: "different-workflow",
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

func TestStagePodUsesFileHostPathTypeForMachineID(t *testing.T) {
	t.Parallel()

	invocation := stageExecutionInvocation()
	invocation.Stage.Test.Security.HostPaths = []signalv1.WorkloadHostPath{{
		Path:      "/etc/machine-id",
		MountPath: "/etc/machine-id",
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
		t.Fatalf("machine-id host path volume = %#v", pod.Spec.Volumes[0])
	}
}

func TestStagePodProvidesBoundedSharedMemoryForIPCLock(t *testing.T) {
	t.Parallel()

	invocation := stageExecutionInvocation()
	invocation.Stage.Test.Security.IPCLock = true
	executor := &KubernetesStageExecutor{
		Namespace:     "signal-system",
		ExecutionName: "stage",
	}

	pod, err := executor.stagePod(testWorkflow("parent", "parent-uid"), invocation, 0)
	if err != nil {
		t.Fatalf("stagePod() error = %v", err)
	}

	if len(pod.Spec.Volumes) != 1 ||
		pod.Spec.Volumes[0].Name != stageSharedMemoryVolumeName ||
		pod.Spec.Volumes[0].EmptyDir == nil ||
		pod.Spec.Volumes[0].EmptyDir.Medium != corev1.StorageMediumMemory ||
		pod.Spec.Volumes[0].EmptyDir.SizeLimit == nil ||
		pod.Spec.Volumes[0].EmptyDir.SizeLimit.String() != stageSharedMemorySize {
		t.Fatalf("stage Pod shared-memory volume = %#v", pod.Spec.Volumes)
	}

	mounts := pod.Spec.Containers[0].VolumeMounts
	if len(mounts) != 1 ||
		mounts[0].Name != stageSharedMemoryVolumeName ||
		mounts[0].MountPath != stageSharedMemoryMountPath {
		t.Fatalf("stage Pod shared-memory mounts = %#v", mounts)
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
	if !strings.Contains(shell, `/bin/sh "$script"`) ||
		!strings.Contains(shell, resultPrefix) {
		t.Fatalf("shell wrapper = %q", shell)
	}

	python := scriptInvocationWrapper(signalv1.ScriptLanguagePython, "check.py", testStageDigest("script"))
	if !strings.Contains(python, `python3 "$script"`) ||
		!strings.Contains(python, `measurements.json`) {
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

func ownedWorkflow(
	name string,
	uid types.UID,
	parent *unstructured.Unstructured,
	runUID string,
) *unstructured.Unstructured {
	workflow := testWorkflow(name, uid)
	workflow.SetLabels(map[string]string{runUIDLabel: runUID})

	controller := true
	blockOwnerDeletion := true
	workflow.SetOwnerReferences([]metav1.OwnerReference{{
		APIVersion:         parent.GetAPIVersion(),
		Kind:               parent.GetKind(),
		Name:               parent.GetName(),
		UID:                parent.GetUID(),
		Controller:         &controller,
		BlockOwnerDeletion: &blockOwnerDeletion,
	}})

	return workflow
}

func workflowChildPod(name string, ownerUID types.UID, restarts int32) *corev1.Pod {
	controller := true
	blockOwnerDeletion := true

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "signal-system",
			UID:       types.UID(name + "-uid"),
			Labels: map[string]string{
				runUIDLabel:       "run-uid",
				argoWorkflowLabel: "child",
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         "argoproj.io/v1alpha1",
				Kind:               "Workflow",
				Name:               "child",
				UID:                ownerUID,
				Controller:         &controller,
				BlockOwnerDeletion: &blockOwnerDeletion,
			}},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:         "main",
				RestartCount: restarts,
			}},
		},
	}
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

	scriptContent := "#!/bin/sh\nexit 0\n"
	scriptDigest := testStageDigest(scriptContent)

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
						Language:    signalv1.ScriptLanguageShell,
						Artifact:    "gpu-inventory.sh",
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
		Artifacts: []signalv1.TestArtifact{{
			Name:    "gpu-inventory.sh",
			Content: scriptContent,
			Digest:  scriptDigest,
		}},
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
		"PROJECT_SIGNAL_RESULTS_DIR":         "/results",
	} {
		if env[name] != want {
			t.Fatalf("%s = %q, want %q", name, env[name], want)
		}
	}

	if pod.Spec.Volumes[1].Name != "results" ||
		pod.Spec.Volumes[1].EmptyDir == nil ||
		pod.Spec.Containers[0].VolumeMounts[1].Name != "results" ||
		pod.Spec.Containers[0].VolumeMounts[1].MountPath != "/results" {
		t.Fatalf("script result volume is not mounted: pod = %#v", pod.Spec)
	}

	if len(pod.Spec.InitContainers) != 1 ||
		pod.Spec.InitContainers[0].Name != "materialize-script" ||
		pod.Spec.Volumes[0].Name != "script" ||
		pod.Spec.Volumes[0].EmptyDir == nil {
		t.Fatalf("inline script materializer is absent: pod = %#v", pod.Spec)
	}

	initEnv := make(map[string]string)
	for _, variable := range pod.Spec.InitContainers[0].Env {
		initEnv[variable.Name] = variable.Value
	}

	decoded, err := base64.StdEncoding.DecodeString(initEnv["PROJECT_SIGNAL_ARTIFACT_B64"])
	if err != nil {
		t.Fatalf("decode materialized artifact: %v", err)
	}

	if string(decoded) != scriptContent ||
		initEnv["PROJECT_SIGNAL_ARTIFACT_NAME"] != "gpu-inventory.sh" ||
		initEnv["PROJECT_SIGNAL_ARTIFACT_DIGEST"] != scriptDigest ||
		!strings.Contains(pod.Spec.InitContainers[0].Args[0], "sha256sum -c -") {
		t.Fatalf("inline script materializer does not preserve artifact identity: %#v", pod.Spec.InitContainers[0])
	}
}

func TestMaximumInlineArtifactFitsLinuxEnvironmentEntry(t *testing.T) {
	t.Parallel()

	const (
		artifactEnvironmentName = "PROJECT_SIGNAL_ARTIFACT_B64"
		linuxMaximumStringBytes = 128 * 1024
	)

	encodedBytes := base64.StdEncoding.EncodedLen(signalv1.MaxTestArtifactBytes)

	entryBytes := len(artifactEnvironmentName) + 1 + encodedBytes + 1
	if entryBytes > linuxMaximumStringBytes {
		t.Fatalf(
			"maximum inline artifact environment entry = %d bytes, Linux maximum = %d",
			entryBytes,
			linuxMaximumStringBytes,
		)
	}
}

func TestParseInvocationResultLogConvertsSimpleScriptResult(t *testing.T) {
	t.Parallel()

	invocation := stageExecutionInvocation()
	driverVersion := "580.1"
	inventoryDigest := testStageDigest("inventory")
	measurements := fmt.Sprintf(
		`{"passed":true,"qualification":{"driverVersion":%q,"gpuInventoryDigest":%q,"visibleDevices":["gpu-index-0"],"coveredDevices":["gpu-index-0"]}}`,
		driverVersion,
		inventoryDigest,
	)
	encode := func(value string) string {
		return base64.StdEncoding.EncodeToString([]byte(value))
	}
	logs := fmt.Sprintf(
		"%s%s %s %s 0\n",
		resultPrefix,
		encode("Pass"),
		encode("completed"),
		encode(measurements),
	)

	envelope, err := parseInvocationResultLog(logs, invocation)
	if err != nil {
		t.Fatalf("parseInvocationResultLog() error = %v", err)
	}

	if envelope.Outcome != signalv1.InvocationOutcomePassed ||
		len(envelope.Participants) != 1 ||
		envelope.Participants[0].Start.DriverVersion != driverVersion ||
		envelope.Participants[0].Start.GPUInventoryDigest != inventoryDigest ||
		len(envelope.Measurements) != 1 ||
		envelope.Measurements[0].Name != "passed" {
		t.Fatalf("converted simple result = %#v", envelope)
	}
}
