// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package gantry

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/operator/component"
	"github.com/Azure/unbounded/internal/registryauth"
)

func TestExternalAuthUpgradeIgnoresUnownedHelperNames(t *testing.T) {
	objects, _ := registryAuthObjects(&component.Env{Namespace: component.DefaultNamespace}, nil, "unused")
	for _, policy := range []string{"", "mode: external"} {
		for _, object := range objects {
			for name, labels := range map[string]map[string]string{
				"unlabeled":                  nil,
				"customer":                   {"app.kubernetes.io/name": registryAuthName, "app.kubernetes.io/managed-by": "customer"},
				"another operator component": {"app.kubernetes.io/name": "another-component", "app.kubernetes.io/managed-by": installationManagerOperator},
			} {
				t.Run(fmt.Sprintf("%s/%s/%s", policy, object.GetObjectKind().GroupVersionKind().Kind, name), func(t *testing.T) {
					source := authSource("mode: external")

					source.Data["config.yaml"] += "    credentials_path: /customer/credentials\n"
					if policy == "" {
						delete(source.Data, registryauth.ConfigKey)
					}

					foreign := component.ToUnstructured(object)
					foreign.SetLabels(labels)
					env := testEnv(t, source, foreign)
					operations, result, err := planRegistryAuth(t.Context(), env)
					require.NoError(t, err)
					require.Empty(t, operations)
					require.Nil(t, result)

					plan, _, err := (Component{}).Plan(t.Context(), env, []unboundedv1alpha3.Site{*siteWithGantry("existing-flex", nil)})
					require.NoError(t, err)
					order, err := plan.ExecutionOrder()
					require.NoError(t, err)
					require.NotContains(t, order, registryAuthName)
					require.NotContains(t, order, registryauth.RestartLeaseName)
					require.NotContains(t, order, registryauth.IdentityRoleName)

					var got corev1.ConfigMap
					require.NoError(t, env.Client.Get(t.Context(), componentKey(configName), &got))
					require.Equal(t, source.Data, got.Data)
				})
			}
		}
	}
}

func TestRegistryAuthRefusesUnownedSupportResources(t *testing.T) {
	objects, _ := registryAuthObjects(&component.Env{Namespace: component.DefaultNamespace}, nil, "unused")
	for _, cleanup := range []bool{false, true} {
		for _, object := range objects {
			t.Run(fmt.Sprintf("cleanup=%t/%s", cleanup, object.GetObjectKind().GroupVersionKind().Kind), func(t *testing.T) {
				source := authSource("")
				foreign := component.ToUnstructured(object)
				foreign.SetLabels(map[string]string{"app.kubernetes.io/managed-by": "customer"})

				fixtures := []client.Object{source, foreign}
				if cleanup {
					source.Data[registryauth.ConfigKey] = "mode: external"

					if foreign.GetKind() == "DaemonSet" {
						fixtures = append(fixtures, &corev1.ConfigMap{ObjectMeta: ownedAuthMetadata()})
					} else {
						fixtures = append(fixtures, &appsv1.DaemonSet{ObjectMeta: ownedAuthMetadata()})
					}
				}

				env := testEnv(t, fixtures...)
				operations, result, err := planRegistryAuth(t.Context(), env)
				require.ErrorContains(t, err, "unowned registry-auth "+foreign.GetKind())
				require.Nil(t, operations)
				require.Nil(t, result)

				got := foreign.DeepCopy()
				require.NoError(t, env.Client.Get(t.Context(), client.ObjectKeyFromObject(foreign), got))
				require.Equal(t, foreign.GetLabels(), got.GetLabels())
			})
		}
	}
}

func TestRegistryAuthPinsOwnedUpdatesAndPreservesRestartLease(t *testing.T) {
	objects, _ := registryAuthObjects(&component.Env{Namespace: component.DefaultNamespace}, nil, "old-config")

	var originalLease *coordinationv1.Lease

	for _, object := range objects {
		object.SetUID(types.UID("owned-" + object.GetObjectKind().GroupVersionKind().Kind))

		if lease, ok := object.(*coordinationv1.Lease); ok {
			holder := "running-installer"
			lease.Spec.HolderIdentity = &holder
			originalLease = lease
		}
	}

	fixtures := append([]client.Object{authSource("")}, objects...)
	env := testEnv(t, fixtures...)
	operations, _, err := planRegistryAuth(t.Context(), env)
	require.NoError(t, err)
	require.Len(t, operations, 7)

	for _, op := range operations {
		require.NotEqual(t, "Lease", op.Object.GetKind())

		if op.Object.GetKind() == "ClusterRole" || op.Object.GetKind() == "ClusterRoleBinding" {
			require.Equal(t, component.OpCreateIfAbsent, op.Kind)
			continue
		}

		require.Equal(t, component.OpApply, op.Kind)
		require.Equal(t, "owned-"+op.Object.GetKind(), string(op.Object.GetUID()))
		require.NotEmpty(t, op.Object.GetResourceVersion())

		if op.Object.GetKind() == "DaemonSet" {
			require.Len(t, op.DependsOn, 6)
		}
	}

	var lease coordinationv1.Lease
	require.NoError(t, env.Client.Get(t.Context(), client.ObjectKeyFromObject(originalLease), &lease))
	require.Equal(t, originalLease.Spec, lease.Spec)
}

func TestRegistryAuthCreateRacePreservesCustomerObject(t *testing.T) {
	env := testEnv(t, authSource(""))
	operations, _, err := planRegistryAuth(t.Context(), env)
	require.NoError(t, err)

	for _, op := range operations {
		require.Equal(t, component.OpCreateIfAbsent, op.Kind)
	}

	foreign := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: registryAuthName, Namespace: env.Namespace, Labels: map[string]string{"app.kubernetes.io/managed-by": "customer"}},
		Data:       map[string]string{"config.json": "preserve"},
	}
	require.NoError(t, env.Client.Create(t.Context(), foreign))

	plan := component.NewPlan()
	plan.Add(operations...)
	result, err := env.Execute(t.Context(), plan)
	require.NoError(t, err)
	require.NotEmpty(t, result.Stale)
	require.NotEmpty(t, result.Deferred)

	var got corev1.ConfigMap
	require.NoError(t, env.Client.Get(t.Context(), client.ObjectKeyFromObject(foreign), &got))
	require.Equal(t, foreign.Data, got.Data)
	require.Equal(t, foreign.Labels, got.Labels)

	var ds appsv1.DaemonSet
	require.True(t, apierrors.IsNotFound(env.Client.Get(t.Context(), client.ObjectKey{Namespace: env.Namespace, Name: registryAuthName}, &ds)))
}
