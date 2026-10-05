// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/unbounded/internal/racer/workload"
)

func TestMixedPlannerMigration(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		t.Run(map[bool]string{false: "toPod", true: "toHost"}[reverse], func(t *testing.T) {
			env := testEnv(t, cache("cache"))
			initialize(t, env)

			for i, node := range []string{"node-a", "node-b"} {
				pod := gantrySocketPod(t, env, "/run/racer")
				pod.Name += string(rune('a' + i))
				pod.Spec.NodeName = node
				require.NoError(t, env.Client.Create(t.Context(), pod))
			}

			cm := &corev1.ConfigMap{}
			require.NoError(t, env.Client.Get(t.Context(), objectKey(env, configName), cm))

			cm.Data["RACER_HOST_NETWORK"] = "true"
			if reverse {
				cm.Data["RACER_POD_NETWORK_NODES"] = `["node-a","node-b"]`
			}

			require.NoError(t, env.Client.Update(t.Context(), cm))
			persist(t, env, planPass(t, env))

			sourceName := dataplaneName
			if reverse {
				sourceName = workload.PodNetworkDaemonSetName
			}

			source := &appsv1.DaemonSet{}
			require.NoError(t, env.Client.Get(t.Context(), objectKey(env, sourceName), source))
			source.UID = types.UID(sourceName)
			require.NoError(t, env.Client.Update(t.Context(), source))
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "source", Namespace: env.Namespace, Finalizers: []string{"test/drain"}, OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "DaemonSet", Name: sourceName, UID: source.UID, Controller: ptr.To(true)}}}, Spec: corev1.PodSpec{NodeName: "node-a"}}
			require.NoError(t, env.Client.Create(t.Context(), pod))

			if reverse {
				delete(cm.Data, "RACER_POD_NETWORK_NODES")
			} else {
				cm.Data["RACER_POD_NETWORK_NODES"] = `["node-a","node-b"]`
			}

			require.NoError(t, env.Client.Update(t.Context(), cm))

			destination := dataplaneName
			if !reverse {
				destination = workload.PodNetworkDaemonSetName
			}

			for pass := 0; pass < 3; pass++ {
				if pass == 1 {
					require.NoError(t, env.Client.Delete(t.Context(), pod))
				}

				plan, result, err := (Component{}).Plan(t.Context(), env, nil)
				require.NoError(t, err)
				require.False(t, result.Ready)
				persist(t, env, plan)

				ds := &appsv1.DaemonSet{}
				require.NoError(t, env.Client.Get(t.Context(), objectKey(env, destination), ds))
				require.False(t, permitsNode(ds, "node-a"), "source, including terminating source, blocks admission after restart")
				require.NoError(t, env.Client.Get(t.Context(), objectKey(env, sourceName), ds))
				require.Equal(t, source.UID, ds.UID)
			}

			require.NoError(t, env.Client.Get(t.Context(), client.ObjectKeyFromObject(pod), pod))
			pod.Finalizers = nil
			require.NoError(t, env.Client.Update(t.Context(), pod))

			for range 2 {
				persist(t, env, planPass(t, env))
			}

			ds := &appsv1.DaemonSet{}
			require.NoError(t, env.Client.Get(t.Context(), objectKey(env, destination), ds))
			require.True(t, permitsNode(ds, "node-a"))
		})
	}
}

type failingMigrationReader struct{ client.Reader }

func (r failingMigrationReader) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return errors.New("pod read denied")
}

func TestMixedPlannerStaleAndReadFailure(t *testing.T) {
	env := testEnv(t, cache("cache"))
	initialize(t, env)
	persist(t, env, planPass(t, env))

	cm := &corev1.ConfigMap{}
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, configName), cm))

	cfg, err := workload.ConfigFromLookup(func(key string) (string, bool) {
		if key == "POD_NAMESPACE" {
			return env.Namespace, true
		}

		v, ok := cm.Data[key]

		return v, ok
	})
	require.NoError(t, err)

	cfg.HostNetwork = true
	cfg.PodNetworkNodes = []string{"node-a", "node-b"}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "stale", Namespace: env.Namespace, OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "DaemonSet", Name: dataplaneName, UID: "stale", Controller: ptr.To(true)}}}, Spec: corev1.PodSpec{NodeName: "node-a"}}
	require.NoError(t, env.Client.Create(t.Context(), pod))
	sets, result, err := migrationPlan(t.Context(), env, cfg)
	require.NoError(t, err)
	require.Equal(t, "MigrationBlocked", result.Reason)

	for _, ds := range sets {
		require.False(t, permitsNode(ds, "node-a"))
	}

	env.APIReader = failingMigrationReader{env.APIReader}
	sets, _, err = migrationPlan(t.Context(), env, cfg)
	require.ErrorContains(t, err, "pod read denied")
	require.Nil(t, sets)
}
