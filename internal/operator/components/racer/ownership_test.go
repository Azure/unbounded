// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

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

				switch phase {
				case "create-race":
					require.Len(t, result.Stale, 1)
				case "ownership-edit":
					// UID-only SSA can restore ownership edited after the live read.
					require.Len(t, result.Results, 1)
					require.Equal(t, component.OpSucceeded, result.Results[0].Status)
					require.NoError(t, env.Client.Get(t.Context(), client.ObjectKeyFromObject(standalone), standalone))
					require.Equal(t, before.GetUID(), standalone.GetUID())
					require.NoError(t, validateRuntimeOwner(standalone, "installation"))
					// An ownership edit seen before planning still blocks adoption.
					standalone.SetAnnotations(nil)
					require.NoError(t, env.Client.Update(t.Context(), standalone))
					before = standalone.DeepCopy()
				default:
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

func TestRuntimeStatusWriteBeforeApply(t *testing.T) {
	testRuntimeStatusWriteBeforeApply(t, testEnv(t))
}

func TestEnvtestRuntimeStatusWriteBeforeApply(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("set KUBEBUILDER_ASSETS for real API ownership tests")
	}

	environment := &envtest.Environment{BinaryAssetsDirectory: assets}
	config, err := environment.Start()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, environment.Stop()) })

	env := testEnv(t)
	env.Client, err = client.New(config, client.Options{Scheme: env.Scheme})
	require.NoError(t, err)

	env.APIReader = env.Client
	require.NoError(t, env.Client.Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: env.Namespace}}))
	testRuntimeStatusWriteBeforeApply(t, env)
}

func testRuntimeStatusWriteBeforeApply(t *testing.T, env *component.Env) {
	t.Helper()

	for _, candidate := range runtimeCandidates(t) {
		statusField := ""

		switch candidate.GetKind() {
		case "Deployment":
			statusField = "replicas"
		case "PodDisruptionBudget":
			statusField = "currentHealthy"
		default:
			continue
		}

		t.Run(candidate.GetKind(), func(t *testing.T) {
			first, err := ownedRuntimeOperation(t.Context(), env, component.Operation{Kind: component.OpApply, Component: name, Object: candidate.DeepCopy()}, "installation")
			require.NoError(t, err)
			require.Equal(t, component.OpCreateIfAbsent, first.Kind)

			plan := component.NewPlan()
			plan.Add(first)
			persist(t, env, plan)

			current := candidate.DeepCopy()
			require.NoError(t, env.Client.Get(t.Context(), client.ObjectKeyFromObject(current), current))

			desired := candidate.DeepCopy()
			desired.SetLabels(map[string]string{"ownership-test": "updated"})
			desired.SetResourceVersion(current.GetResourceVersion())
			op, err := ownedRuntimeOperation(t.Context(), env, component.Operation{Kind: component.OpApply, Component: name, Object: desired}, "installation")
			require.NoError(t, err)
			require.Equal(t, component.OpApply, op.Kind)
			require.Equal(t, current.GetUID(), op.Object.GetUID())

			observedRevision := current.GetResourceVersion()
			require.NoError(t, unstructured.SetNestedField(current.Object, int64(2), "status", statusField))
			require.NoError(t, env.Client.Status().Update(t.Context(), current))
			require.NotEqual(t, observedRevision, current.GetResourceVersion())

			plan = component.NewPlan()
			plan.Add(op)
			persist(t, env, plan)
			require.Empty(t, op.Object.GetResourceVersion())
			require.NoError(t, env.Client.Get(t.Context(), client.ObjectKeyFromObject(current), current))
			require.Equal(t, "updated", current.GetLabels()["ownership-test"])
			status, found, err := unstructured.NestedInt64(current.Object, "status", statusField)
			require.NoError(t, err)
			require.True(t, found)
			require.Equal(t, int64(2), status)

			require.NoError(t, env.Client.Delete(t.Context(), current))

			replacement := candidate.DeepCopy()
			require.NoError(t, env.Client.Create(t.Context(), replacement))
			require.NoError(t, env.Client.Get(t.Context(), client.ObjectKeyFromObject(replacement), replacement))
			require.NotEqual(t, op.Object.GetUID(), replacement.GetUID())
			before := replacement.DeepCopy()
			result, err := env.Execute(t.Context(), plan)
			require.NoError(t, err)
			require.Len(t, result.Results, 1)
			require.NotEqual(t, component.OpSucceeded, result.Results[0].Status)
			require.True(t, apierrors.IsConflict(result.Results[0].Err) || apierrors.IsInvalid(result.Results[0].Err), "%v", result.Results[0].Err)
			t.Logf("replacement rejected: %v", result.Results[0].Err)
			require.NoError(t, env.Client.Get(t.Context(), client.ObjectKeyFromObject(replacement), replacement))
			require.Equal(t, before, replacement)
		})
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
