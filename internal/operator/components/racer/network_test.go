// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"testing"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/Azure/unbounded/internal/operator/component"
	racercore "github.com/Azure/unbounded/internal/racer"
)

func TestRacerNetworkConfiguration(t *testing.T) {
	env := testEnv(t, cache("cache"))
	initialize(t, env)
	persist(t, env, planPass(t, env))

	cm := &corev1.ConfigMap{}
	ds := &appsv1.DaemonSet{}
	deployment := &appsv1.Deployment{}

	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, configName), cm))
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, dataplaneName), ds))
	require.False(t, ds.Spec.Template.Spec.HostNetwork)
	require.Equal(t, corev1.DNSClusterFirst, ds.Spec.Template.Spec.DNSPolicy)
	require.Equal(t, int32(8082), ds.Spec.Template.Spec.Containers[0].Ports[0].ContainerPort)
	require.Equal(t, int32(9090), ds.Spec.Template.Spec.Containers[0].Ports[1].ContainerPort)
	baseline := ds.Spec.Template.Spec.DeepCopy()

	cm.Data["RACER_HOST_NETWORK"] = "true"
	cm.Data["RACER_PEER_PORT"] = "18082"
	cm.Data["RACER_DIAGNOSTICS_PORT"] = "19090"
	require.NoError(t, env.Client.Update(t.Context(), cm))
	persist(t, env, planPass(t, env))
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, dataplaneName), ds))
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, controllerName), deployment))
	require.Equal(t, component.ConfigMapPayloadHash(cm), deployment.Spec.Template.Annotations["unbounded-cloud.io/racer-config-hash"])
	require.Equal(t, configName, deployment.Spec.Template.Spec.Containers[0].EnvFrom[0].ConfigMapRef.Name)
	require.False(t, deployment.Spec.Template.Spec.HostNetwork)

	pod := ds.Spec.Template.Spec
	require.True(t, pod.HostNetwork)
	require.Equal(t, corev1.DNSClusterFirstWithHostNet, pod.DNSPolicy)
	require.False(t, pod.HostPID)
	require.False(t, pod.HostIPC)
	require.Equal(t, baseline.Containers[0].SecurityContext, pod.Containers[0].SecurityContext)
	require.Equal(t, baseline.Volumes, pod.Volumes)
	require.Equal(t, baseline.ServiceAccountName, pod.ServiceAccountName)
	require.Zero(t, ds.Spec.UpdateStrategy.RollingUpdate.MaxSurge.IntValue())
	require.Equal(t, "diagnostics", pod.Containers[0].ReadinessProbe.HTTPGet.Port.StrVal)

	values := map[string]corev1.EnvVar{}
	for _, variable := range pod.Containers[0].Env {
		values[variable.Name] = variable
	}

	require.Equal(t, "status.podIP", values["RACER_POD_IP"].ValueFrom.FieldRef.FieldPath)
	require.Equal(t, "[$(RACER_POD_IP)]:18082", values["RACER_PEER_LISTEN"].Value)
	require.Equal(t, "[$(RACER_POD_IP)]:19090", values["RACER_DIAGNOSTICS_LISTEN"].Value)
	require.Equal(t, int32(18082), pod.Containers[0].Ports[0].ContainerPort)
	require.Equal(t, int32(19090), pod.Containers[0].Ports[1].ContainerPort)

	// The existing controller parser ignores workload-only keys and publishes
	// the same port. Ownership remains mandatory even on the host network.
	cfg := configuration(t, env)
	// The fake SSA persistence path does not allocate API server UIDs.
	ds.UID = "managed-daemonset"

	for _, ip := range []string{"10.0.0.12", "fd00::12"} {
		managed := corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{UID: "pod", OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "DaemonSet", UID: ds.UID, Controller: ptr.To(true)}}},
			Spec:       pod, Status: corev1.PodStatus{PodIP: ip},
		}
		managed.Spec.NodeName = "node"
		endpoint, err := racercore.SelectEndpoint([]corev1.Pod{managed}, ds.UID, "node", cfg.PeerPort)
		require.NoError(t, err)

		want := ip + ":18082"
		if ip == "fd00::12" {
			want = "[" + ip + "]:18082"
		}

		require.Equal(t, want, endpoint)

		managed.OwnerReferences = nil
		_, err = racercore.SelectEndpoint([]corev1.Pod{managed}, ds.UID, "node", cfg.PeerPort)
		require.Error(t, err)
	}

	// Removing the opt-in restores ordinary Pod networking and legacy ports.
	for _, key := range []string{"RACER_HOST_NETWORK", "RACER_PEER_PORT", "RACER_DIAGNOSTICS_PORT"} {
		delete(cm.Data, key)
	}

	require.NoError(t, env.Client.Update(t.Context(), cm))
	persist(t, env, planPass(t, env))
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, dataplaneName), ds))
	require.Equal(t, *baseline, ds.Spec.Template.Spec)
}

func TestRacerNetworkRejectsInvalidConfiguration(t *testing.T) {
	env := testEnv(t, cache("cache"))
	initialize(t, env)
	persist(t, env, planPass(t, env))

	for key, invalid := range map[string][]string{
		"RACER_HOST_NETWORK":     {"", "TRUE", "1", "yes"},
		"RACER_PEER_PORT":        {"", "0", "1023", "65536", "-1", "18082x"},
		"RACER_DIAGNOSTICS_PORT": {"", "0", "1023", "65536", "-1", "8082"},
	} {
		for _, value := range invalid {
			t.Run(key+"="+value, func(t *testing.T) {
				cm := &corev1.ConfigMap{}
				require.NoError(t, env.Client.Get(t.Context(), objectKey(env, configName), cm))
				before := cm.DeepCopy()
				cm.Data[key] = value
				require.NoError(t, env.Client.Update(t.Context(), cm))
				plan, _, err := (Component{}).Plan(t.Context(), env, nil)
				require.Error(t, err)
				require.Nil(t, plan, "invalid network configuration must not apply a partial plan")

				cm.Data = before.Data
				require.NoError(t, env.Client.Update(t.Context(), cm))
			})
		}
	}
}
