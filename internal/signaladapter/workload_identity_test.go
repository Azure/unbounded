// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signaladapter

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	signalv1 "github.com/Azure/unbounded/api/signal/v1alpha1"
)

func TestLegacyPodBackendsUseFixedNonRootIdentity(t *testing.T) {
	t.Parallel()

	config := Config{
		Namespace:       "signal-system",
		ExecutionName:   "execution",
		RunName:         "run",
		RunnerImage:     "example.invalid/runner@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		ScriptConfigMap: "script",
	}
	participant := signalv1.Participant{Index: 0, NodeName: "worker", NodeUID: "node-uid"}
	owner := metav1.OwnerReference{APIVersion: "argoproj.io/v1alpha1", Kind: "Workflow", Name: "run"}

	pod := ordinaryPod(config, participant, nil, nil, signalv1.WorkloadPolicy{}, owner, 60, 0)
	assertFixedPodIdentity(t, pod.Spec)

	resource, err := mpiJob(
		config,
		[]signalv1.Participant{participant},
		nil,
		nil,
		signalv1.WorkloadPolicy{},
		owner,
		60,
		1,
	)
	if err != nil {
		t.Fatalf("mpiJob() error = %v", err)
	}

	for _, role := range []string{"Launcher", "Worker"} {
		template, found, nestedErr := unstructured.NestedMap(
			resource.Object,
			"spec",
			"mpiReplicaSpecs",
			role,
			"template",
		)
		if nestedErr != nil || !found {
			t.Fatalf("%s template found %t, error %v", role, found, nestedErr)
		}

		var typed corev1.PodTemplateSpec
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(template, &typed); err != nil {
			t.Fatalf("decode %s template: %v", role, err)
		}

		assertFixedPodIdentity(t, typed.Spec)
	}
}

func TestWorkflowTemplateRuntimeIdentityOverridesRootAndCoversMPIReplicas(t *testing.T) {
	t.Parallel()

	spec := map[string]any{
		"templates": []any{
			map[string]any{
				"name": "container",
				"container": map[string]any{
					"image": "example.invalid/workload@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
					"securityContext": map[string]any{
						"privileged":   true,
						"runAsUser":    int64(0),
						"runAsGroup":   int64(0),
						"runAsNonRoot": false,
					},
				},
			},
			map[string]any{
				"name": "mpi",
				"resource": map[string]any{
					"manifest": `{
						"apiVersion":"kubeflow.org/v2beta1",
						"kind":"MPIJob",
						"spec":{"mpiReplicaSpecs":{
							"Launcher":{"template":{"spec":{"containers":[{"name":"launcher","image":"example.invalid/runner"}]}}},
							"Worker":{"template":{"spec":{"containers":[{"name":"worker","image":"example.invalid/runner"}]}}}
						}}
					}`,
				},
			},
		},
	}

	if err := applyWorkflowRuntimeIdentity(spec); err != nil {
		t.Fatalf("applyWorkflowRuntimeIdentity() error = %v", err)
	}

	templates := spec["templates"].([]any)
	container := templates[0].(map[string]any)["container"].(map[string]any)

	securityContext := container["securityContext"].(map[string]any)
	if securityContext["runAsUser"] != int64(workloadIdentityID) ||
		securityContext["runAsGroup"] != int64(workloadIdentityID) ||
		securityContext["runAsNonRoot"] != true ||
		securityContext["privileged"] != true {
		t.Fatalf("WorkflowTemplate container security context = %#v", securityContext)
	}

	resourceTemplate := templates[1].(map[string]any)["resource"].(map[string]any)

	object, _, err := unstructured.UnstructuredJSONScheme.Decode(
		[]byte(resourceTemplate["manifest"].(string)),
		nil,
		nil,
	)
	if err != nil {
		t.Fatalf("decode normalized MPIJob: %v", err)
	}

	resource := object.(*unstructured.Unstructured).Object
	for _, role := range []string{"Launcher", "Worker"} {
		podSpec, found, nestedErr := unstructured.NestedMap(
			resource,
			"spec",
			"mpiReplicaSpecs",
			role,
			"template",
			"spec",
		)
		if nestedErr != nil || !found {
			t.Fatalf("%s Pod spec found %t, error %v", role, found, nestedErr)
		}

		var typed corev1.PodSpec
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(podSpec, &typed); err != nil {
			t.Fatalf("decode %s Pod spec: %v", role, err)
		}

		assertFixedPodIdentity(t, typed)
	}
}

func TestWorkflowTemplateRuntimeIdentityRejectsMalformedResource(t *testing.T) {
	t.Parallel()

	spec := map[string]any{
		"templates": []any{map[string]any{
			"resource": map[string]any{"manifest": "not: [valid"},
		}},
	}

	if err := applyWorkflowRuntimeIdentity(spec); err == nil {
		t.Fatal("applyWorkflowRuntimeIdentity() accepted a malformed resource manifest")
	}
}

func TestWorkflowTemplateRuntimeIdentityRejectsUnsupportedResource(t *testing.T) {
	t.Parallel()

	spec := map[string]any{
		"templates": []any{map[string]any{
			"resource": map[string]any{
				"manifest": `{"apiVersion":"example.invalid/v1","kind":"CustomWorkload"}`,
			},
		}},
	}

	if err := applyWorkflowRuntimeIdentity(spec); err == nil {
		t.Fatal("applyWorkflowRuntimeIdentity() accepted an unsupported resource kind")
	}
}

func assertFixedPodIdentity(t *testing.T, spec corev1.PodSpec) {
	t.Helper()

	if spec.SecurityContext == nil ||
		spec.SecurityContext.RunAsUser == nil ||
		*spec.SecurityContext.RunAsUser != workloadIdentityID ||
		spec.SecurityContext.RunAsGroup == nil ||
		*spec.SecurityContext.RunAsGroup != workloadIdentityID ||
		spec.SecurityContext.RunAsNonRoot == nil ||
		!*spec.SecurityContext.RunAsNonRoot ||
		spec.SecurityContext.FSGroup == nil ||
		*spec.SecurityContext.FSGroup != workloadIdentityID {
		t.Fatalf("Pod security context = %#v", spec.SecurityContext)
	}

	for index := range spec.InitContainers {
		assertFixedContainerIdentity(t, spec.InitContainers[index])
	}

	for index := range spec.Containers {
		assertFixedContainerIdentity(t, spec.Containers[index])
	}
}

func assertFixedContainerIdentity(t *testing.T, container corev1.Container) {
	t.Helper()

	if container.SecurityContext == nil ||
		container.SecurityContext.RunAsUser == nil ||
		*container.SecurityContext.RunAsUser != workloadIdentityID ||
		container.SecurityContext.RunAsGroup == nil ||
		*container.SecurityContext.RunAsGroup != workloadIdentityID ||
		container.SecurityContext.RunAsNonRoot == nil ||
		!*container.SecurityContext.RunAsNonRoot {
		t.Fatalf("%s security context = %#v", container.Name, container.SecurityContext)
	}
}
