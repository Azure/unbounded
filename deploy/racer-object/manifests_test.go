// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racerobject_test

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

func readExample(t *testing.T, name string, object any) {
	t.Helper()

	data, err := os.ReadFile(name)
	require.NoError(t, err)
	require.NoError(t, yaml.UnmarshalStrict(data, object))
}

func TestClusterVolumeExample(t *testing.T) {
	var volume map[string]any
	readExample(t, "volume.yaml", &volume)
	require.Equal(t, map[string]any{
		"apiVersion": "racer.unbounded-cloud.io/v1alpha1",
		"kind":       "ClusterVolume",
		"metadata":   map[string]any{"name": "racer-object"},
		"spec":       map[string]any{"type": "Cache"},
	}, volume)
}

func TestOriginExample(t *testing.T) {
	var ds appsv1.DaemonSet
	readExample(t, "origin.yaml", &ds)
	require.Equal(t, "apps/v1", ds.APIVersion)
	require.Equal(t, "DaemonSet", ds.Kind)
	require.Equal(t, ds.Spec.Selector.MatchLabels, ds.Spec.Template.Labels)
	require.Equal(t, int32(0), ds.Spec.UpdateStrategy.RollingUpdate.MaxSurge.IntVal)

	pod := ds.Spec.Template.Spec
	require.Len(t, pod.Containers, 1)
	checkPod(t, pod)
	require.Equal(t, int64(0), *pod.SecurityContext.RunAsUser)
	require.Equal(t, int64(0), *pod.SecurityContext.RunAsGroup)
	require.False(t, *pod.SecurityContext.RunAsNonRoot)

	origin := pod.Containers[0]
	require.Equal(t, "origin", origin.Name)
	require.Equal(t, []string{
		"origin", "--volume=racer-object", "--namespace=example-store", "--bucket=example-bucket",
		"--region=us-east-1", "--path-style=true", "--metadata-ttl=30s", "--request-timeout=1m",
	}, origin.Args)
	require.Empty(t, origin.Env)
	require.Len(t, origin.EnvFrom, 1)
	require.Nil(t, origin.EnvFrom[0].ConfigMapRef)
	require.Equal(t, "racer-object-aws", origin.EnvFrom[0].SecretRef.Name)
	require.True(t, *origin.EnvFrom[0].SecretRef.Optional)
	checkSocket(t, pod, origin, "origin", corev1.HostPathDirectoryOrCreate, false)
}

func TestSidecarExample(t *testing.T) {
	var pod corev1.Pod
	readExample(t, "sidecar-pod.yaml", &pod)
	require.Equal(t, "v1", pod.APIVersion)
	require.Equal(t, "Pod", pod.Kind)
	require.Len(t, pod.Spec.Containers, 2)
	checkPod(t, pod.Spec)

	app, sidecar := pod.Spec.Containers[0], pod.Spec.Containers[1]
	require.Equal(t, "app", app.Name)
	require.True(t, *app.SecurityContext.RunAsNonRoot)
	require.Empty(t, app.VolumeMounts)
	require.Empty(t, app.EnvFrom)
	require.Empty(t, app.Env)
	require.Equal(t, "sidecar", sidecar.Name)
	require.Equal(t, int64(0), *sidecar.SecurityContext.RunAsUser)
	require.Equal(t, int64(0), *sidecar.SecurityContext.RunAsGroup)
	require.False(t, *sidecar.SecurityContext.RunAsNonRoot)
	require.Equal(t, []string{
		"sidecar", "--volume=racer-object", "--namespace=example-store", "--bucket=example-bucket",
		"--listen=127.0.0.1:8080", "--request-timeout=5m",
	}, sidecar.Args)
	require.Empty(t, sidecar.EnvFrom)
	require.Empty(t, sidecar.Env)
	checkSocket(t, pod.Spec, sidecar, "client", corev1.HostPathDirectory, true)

	var ds appsv1.DaemonSet
	readExample(t, "origin.yaml", &ds)
	require.Equal(t, ds.Spec.Template.Spec.Containers[0].Image, sidecar.Image)
	require.Equal(t, ds.Spec.Template.Spec.NodeSelector, pod.Spec.NodeSelector)
	require.Equal(t, ds.Spec.Template.Spec.Affinity, pod.Spec.Affinity)
	require.Equal(t, ds.Spec.Template.Spec.Tolerations, pod.Spec.Tolerations)
}

func checkPod(t *testing.T, pod corev1.PodSpec) {
	t.Helper()

	require.False(t, *pod.AutomountServiceAccountToken)
	require.False(t, pod.HostNetwork)
	require.False(t, pod.HostPID)
	require.False(t, pod.HostIPC)
	require.Empty(t, pod.InitContainers)
	require.Equal(t, map[string]string{"kubernetes.io/os": "linux"}, pod.NodeSelector)
	require.Equal(t, []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{{
		Key: "racer.unbounded-cloud.io/exclude", Operator: corev1.NodeSelectorOpDoesNotExist,
	}}}}, pod.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms)
	require.Equal(t, corev1.SeccompProfileTypeRuntimeDefault, pod.SecurityContext.SeccompProfile.Type)

	for _, container := range pod.Containers {
		require.False(t, *container.SecurityContext.Privileged)
		require.False(t, *container.SecurityContext.AllowPrivilegeEscalation)
		require.True(t, *container.SecurityContext.ReadOnlyRootFilesystem)
		require.Equal(t, []corev1.Capability{"ALL"}, container.SecurityContext.Capabilities.Drop)
		require.Empty(t, container.SecurityContext.Capabilities.Add)
		require.Empty(t, container.Ports)
		require.Nil(t, container.ReadinessProbe)
		require.Nil(t, container.LivenessProbe)

		for _, resource := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
			request, limit := container.Resources.Requests[resource], container.Resources.Limits[resource]
			require.True(t, request.Sign() > 0)
			require.True(t, limit.Cmp(request) >= 0)
		}
	}
}

func checkSocket(t *testing.T, pod corev1.PodSpec, container corev1.Container, role string, pathType corev1.HostPathType, readOnly bool) {
	t.Helper()

	path := "/run/racer/racer-object/" + role
	require.Equal(t, []corev1.VolumeMount{{Name: role, MountPath: path, ReadOnly: readOnly}}, container.VolumeMounts)
	require.Len(t, pod.Volumes, 1)
	require.Equal(t, role, pod.Volumes[0].Name)
	require.Equal(t, corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: path, Type: &pathType}}, pod.Volumes[0].VolumeSource)
}
