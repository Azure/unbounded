// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package netboot

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

func TestBootstrapArtifactRequests(t *testing.T) {
	t.Parallel()

	require.Empty(t, (&OCIReconciler{}).bootstrapArtifactRequests("amd64"))
	require.Equal(t, []imagePullRequest{{
		ImageRef:     "ghcr.io/test/bootstrap-artifacts:v1-k8s-v1.34.2",
		Architecture: "amd64",
	}}, (&OCIReconciler{
		BootstrapArtifactsRef: "ghcr.io/test/bootstrap-artifacts:v1-k8s-v1.34.2",
	}).bootstrapArtifactRequests("amd64"))
}

func TestDaemonSetRequestsCollectsImages(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))

	daemonSet := func(name string, nodeSelector map[string]string, spec corev1.PodSpec) *appsv1.DaemonSet {
		return &appsv1.DaemonSet{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "kube-system"},
			Spec: appsv1.DaemonSetSpec{
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						NodeSelector:     nodeSelector,
						ImagePullSecrets: spec.ImagePullSecrets,
						InitContainers:   spec.InitContainers,
						Containers:       spec.Containers,
					},
				},
			},
		}
	}

	objs := []runtime.Object{
		daemonSet("unbounded-net-node", map[string]string{"kubernetes.io/os": "linux"}, corev1.PodSpec{
			InitContainers: []corev1.Container{{Name: "init", Image: "ghcr.io/test/net-node-init:v1"}},
			Containers: []corev1.Container{
				{
					Name:            "agent",
					Image:           "ghcr.io/test/net-node:v1",
					ImagePullPolicy: corev1.PullAlways,
				},
				{Name: "no-pull", Image: "ghcr.io/test/ignored:v1", ImagePullPolicy: corev1.PullNever},
				{Name: "no-image"},
			},
			ImagePullSecrets: []corev1.LocalObjectReference{{Name: "net-node-pull"}},
		}),
		daemonSet("gpu-helper", map[string]string{"kubernetes.io/os": "linux", "nvidia.com/gpu.present": "true"}, corev1.PodSpec{
			Containers: []corev1.Container{{Name: "helper", Image: "ghcr.io/test/gpu-helper:v1"}},
		}),
	}

	reconciler := &OCIReconciler{
		Client:         fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build(),
		ProviderLabels: map[string]string{"kubernetes.io/os": "linux"},
	}

	machine := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-a"},
		Spec: v1alpha3.MachineSpec{
			Kubernetes: &v1alpha3.KubernetesSpec{NodeLabels: map[string]string{"nvidia.com/gpu.present": "true"}},
		},
	}

	require.Equal(t, []imagePullRequest{
		{
			ImageRef:     "ghcr.io/test/gpu-helper:v1",
			Architecture: v1alpha3.DefaultPXEArchitecture,
		},
		{
			ImageRef:      "ghcr.io/test/net-node-init:v1",
			Architecture:  v1alpha3.DefaultPXEArchitecture,
			PullSecretRef: &v1alpha3.NamespacedSecretReference{Namespace: "kube-system", Name: "net-node-pull"},
		},
		{
			ImageRef:      "ghcr.io/test/net-node:v1",
			Architecture:  v1alpha3.DefaultPXEArchitecture,
			PullSecretRef: &v1alpha3.NamespacedSecretReference{Namespace: "kube-system", Name: "net-node-pull"},
		},
	}, reconciler.daemonSetRequests(context.Background(), machine, v1alpha3.DefaultPXEArchitecture))

	// Without the GPU label the GPU DaemonSet is left out.
	delete(machine.Spec.Kubernetes.NodeLabels, "nvidia.com/gpu.present")

	requests := reconciler.daemonSetRequests(context.Background(), machine, v1alpha3.DefaultPXEArchitecture)

	refs := make([]string, 0, len(requests))
	for _, request := range requests {
		refs = append(refs, request.ImageRef)
	}

	require.Equal(t, []string{"ghcr.io/test/net-node-init:v1", "ghcr.io/test/net-node:v1"}, refs)

	// Without a client nothing can be discovered.
	require.Empty(t, (&OCIReconciler{}).daemonSetRequests(context.Background(), machine, v1alpha3.DefaultPXEArchitecture))
}
