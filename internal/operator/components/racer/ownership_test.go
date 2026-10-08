// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/unbounded/internal/operator/component"
)

func runtimeCandidates(t *testing.T) []*unstructured.Unstructured {
	t.Helper()
	env := testEnv(t)
	objects, err := decodeRuntimeManifests(env)
	require.NoError(t, err)
	secret, err := newTLS(env.Namespace, time.Now())
	require.NoError(t, err)

	return append(objects, component.ToUnstructured(secret), component.ToUnstructured(&corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{Name: trustName, Namespace: env.Namespace},
		Data:       map[string]string{"ca.crt": "standalone"},
	}))
}

func TestRuntimeCollisionAfterClaimInventory(t *testing.T) {
	for _, candidate := range runtimeCandidates(t) {
		t.Run(candidate.GetKind()+"/"+candidate.GetName(), func(t *testing.T) {
			env := testEnv(t, cacheObject("cache"))
			for range 2 {
				plan, _ := identityPlan(t, env)
				persist(t, env, plan)
			}

			claimCAS, _ := identityPlan(t, env)
			standalone := candidate.DeepCopy()
			require.NoError(t, env.Client.Create(t.Context(), standalone))
			require.NoError(t, env.Client.Get(t.Context(), client.ObjectKeyFromObject(standalone), standalone))
			before := standalone.DeepCopy()

			persist(t, env, claimCAS)

			failed := false

			for range 6 {
				plan, _, err := (Component{}).Plan(t.Context(), env, nil)
				if err != nil {
					require.Error(t, err)
					require.Nil(t, plan)

					failed = true

					break
				}

				persist(t, env, plan)
			}

			require.True(t, failed, "standalone resource was accepted")
			require.NoError(t, env.Client.Get(t.Context(), client.ObjectKeyFromObject(standalone), standalone))
			require.Equal(t, before, standalone)
		})
	}
}

func TestRuntimeCreateRaceAndReplacement(t *testing.T) {
	for _, candidate := range runtimeCandidates(t) {
		for _, phase := range []string{"create-race", "replacement", "ownership-edit"} {
			t.Run(candidate.GetKind()+"/"+candidate.GetName()+"/"+phase, func(t *testing.T) {
				env := testEnv(t)

				desired := component.Operation{Kind: component.OpApply, Component: name, Object: candidate.DeepCopy()}
				if phase != "create-race" {
					first, err := ownedRuntimeOperation(t.Context(), env, desired, "installation")
					require.NoError(t, err)

					plan := component.NewPlan()
					plan.Add(first)
					persist(t, env, plan)
				}

				op, err := ownedRuntimeOperation(t.Context(), env, component.Operation{Kind: component.OpApply, Component: name, Object: candidate.DeepCopy()}, "installation")
				require.NoError(t, err)

				standalone := candidate.DeepCopy()
				if phase != "create-race" {
					require.NoError(t, env.Client.Get(t.Context(), client.ObjectKeyFromObject(standalone), standalone))
				}

				if phase == "ownership-edit" {
					standalone.SetAnnotations(nil)
					require.NoError(t, env.Client.Update(t.Context(), standalone))
				} else {
					if phase == "replacement" {
						require.NoError(t, env.Client.Delete(t.Context(), standalone))
					}

					standalone = candidate.DeepCopy()
					require.NoError(t, env.Client.Create(t.Context(), standalone))
				}

				require.NoError(t, env.Client.Get(t.Context(), client.ObjectKeyFromObject(standalone), standalone))
				before := standalone.DeepCopy()
				plan := component.NewPlan()
				plan.Add(op)
				result, err := env.Execute(t.Context(), plan)
				require.NoError(t, err)

				if phase == "create-race" {
					require.Len(t, result.Stale, 1)
				} else {
					require.Len(t, result.Deferred, 1)
				}

				require.NoError(t, env.Client.Get(t.Context(), client.ObjectKeyFromObject(standalone), standalone))
				require.Equal(t, before, standalone)
				_, err = ownedRuntimeOperation(t.Context(), env, component.Operation{Kind: component.OpApply, Component: name, Object: candidate.DeepCopy()}, "installation")
				require.ErrorContains(t, err, "refusing adoption")
			})
		}
	}
}

func TestRuntimePayloadReplacementBeforePatch(t *testing.T) {
	for _, target := range []string{configName, tlsName, trustName} {
		for _, retained := range []bool{false, true} {
			t.Run(target+map[bool]string{false: "/active", true: "/retained"}[retained], func(t *testing.T) {
				env := testEnv(t, cacheObject("cache"))
				initialize(t, env)

				if retained {
					require.NoError(t, env.Client.Delete(t.Context(), cacheObject("cache")))
				}

				var obj client.Object = &corev1.ConfigMap{}
				if target == tlsName {
					obj = &corev1.Secret{}
				}

				require.NoError(t, env.Client.Get(t.Context(), objectKey(env, target), obj))

				now := time.Now()

				switch target {
				case configName:
					obj.(*corev1.ConfigMap).Data["RACER_CLUSTER_ID"] = "wrong"
				case tlsName:
					now = stateOf(t, obj.(*corev1.Secret)).CreatedAt.Add(caRotationInterval)
				case trustName:
					obj.(*corev1.ConfigMap).Data["ca.crt"] = "wrong"
				}

				require.NoError(t, env.Client.Update(t.Context(), obj))
				plan, _, err := planAt(t.Context(), env, now)
				require.NoError(t, err)
				require.NoError(t, env.Client.Delete(t.Context(), obj))
				obj.SetResourceVersion("")
				obj.SetAnnotations(nil)
				require.NoError(t, env.Client.Create(t.Context(), obj))
				// Fake revisions restart after deletion. Real API revisions do not.
				for range 3 {
					require.NoError(t, env.Client.Update(t.Context(), obj))
				}

				before := obj.DeepCopyObject()
				result, err := env.Execute(t.Context(), plan)
				require.NoError(t, err)

				if retained && target == configName {
					require.Zero(t, plan.Len())
				} else {
					require.NotEmpty(t, result.Deferred)
				}

				require.NoError(t, env.Client.Get(t.Context(), objectKey(env, target), obj))
				require.Equal(t, before, obj)
			})
		}
	}
}

func TestContainmentDoesNotDeleteReplacedBindings(t *testing.T) {
	env := testEnv(t, cacheObject("cache"))
	initialize(t, env)
	require.NoError(t, env.Client.Delete(t.Context(), &admissionv1.ValidatingAdmissionPolicy{ObjectMeta: metav1.ObjectMeta{Name: guardNames[0]}}))
	plan := planPass(t, env)
	require.Len(t, plan.Operations, 2)

	for _, op := range plan.Operations {
		require.NotEmpty(t, op.Object.GetUID())
		require.NotEmpty(t, op.Object.GetResourceVersion())
	}

	standalone := &rbacv1.RoleBinding{}
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, controllerName), standalone))
	require.NoError(t, env.Client.Delete(t.Context(), standalone))
	standalone.ResourceVersion = ""
	standalone.Annotations = nil
	require.NoError(t, env.Client.Create(t.Context(), standalone))
	before := standalone.DeepCopy()
	result, err := env.Execute(t.Context(), plan)
	require.NoError(t, err)
	require.NotEmpty(t, result.Deferred)
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, controllerName), standalone))
	require.Equal(t, before, standalone)
}
