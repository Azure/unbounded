// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/Azure/unbounded/internal/operator/component"
)

func TestRetainedTLSWeeklyMaintenance(t *testing.T) {
	env := testEnv(t, volume("cache"))
	initialize(t, env)
	persist(t, env, planPass(t, env))
	require.NoError(t, env.Client.Delete(t.Context(), volume("cache")))

	secret := &corev1.Secret{}
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, tlsName), secret))
	now := stateOf(t, secret).CreatedAt
	deployment, ds := &appsv1.Deployment{}, &appsv1.DaemonSet{}
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, controllerName), deployment))
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, dataplaneName), ds))
	beforeController, beforeDataplane := deployment.DeepCopy(), ds.DeepCopy()

	for week := range 9 {
		at := now.Add(time.Duration(week) * caRotationInterval)
		old := secret.DeepCopy()
		plan, result, err := planAt(t.Context(), env, at)
		require.NoError(t, err)
		require.Equal(t, component.ReasonDisabled, result.Reason)
		require.True(t, result.Ready)

		if week == 0 {
			require.Zero(t, plan.Len())
		} else {
			require.Equal(t, 5*time.Second, result.RequeueAfter)
			require.Len(t, plan.Operations, 1)
			require.Equal(t, "Secret", plan.Operations[0].Object.GetKind())
			require.Equal(t, component.OpMergePatch, plan.Operations[0].Kind)
			persist(t, env, plan)
			plan, result, err = planAt(t.Context(), env, at)
			require.NoError(t, err)
			require.Equal(t, 5*time.Second, result.RequeueAfter)
			require.Len(t, plan.Operations, 1)
			require.Equal(t, trustName, plan.Operations[0].Object.GetName())
			require.Equal(t, component.OpMergePatch, plan.Operations[0].Kind)
			persist(t, env, plan)
		}
		// No memory of the prior pass is needed, and idle passes still requeue.
		plan, result, err = planAt(t.Context(), env, at)
		require.NoError(t, err)
		require.Zero(t, plan.Len())
		require.Equal(t, time.Hour, result.RequeueAfter)
		require.NoError(t, env.Client.Get(t.Context(), objectKey(env, tlsName), secret))

		if week > 0 {
			require.NotEqual(t, old.Data["ca.key"], secret.Data["ca.key"])
		}

		require.NoError(t, verifyServing(t, secret, old.Data["ca.crt"], at))
		require.NoError(t, verifyServing(t, old, secret.Data[caBundleKey], at))
		require.Equal(t, string(secret.Data["ca.crt"])+string(secret.Data[previousCAKey]), string(secret.Data[caBundleKey]))

		trust := &corev1.ConfigMap{}
		require.NoError(t, env.Client.Get(t.Context(), objectKey(env, trustName), trust))
		require.Equal(t, string(secret.Data["ca.crt"])+string(secret.Data[previousCAKey]), trust.Data["ca.crt"])
		require.NoError(t, env.Client.Get(t.Context(), objectKey(env, controllerName), deployment))
		require.NoError(t, env.Client.Get(t.Context(), objectKey(env, dataplaneName), ds))
		require.Equal(t, beforeController, deployment)
		require.Equal(t, beforeDataplane, ds)
	}
	// Deliberate removal remains possible, including deleting workloads while
	// leaving the maintained Secret, then removing the Secret itself.
	require.NoError(t, env.Client.Delete(t.Context(), deployment))
	require.NoError(t, env.Client.Delete(t.Context(), ds))
	plan, _, err := planAt(t.Context(), env, now.Add(9*caRotationInterval))
	require.NoError(t, err)
	require.Len(t, plan.Operations, 1)
	persist(t, env, plan)
	require.True(t, apierrors.IsNotFound(env.Client.Get(t.Context(), objectKey(env, controllerName), deployment)))
	require.True(t, apierrors.IsNotFound(env.Client.Get(t.Context(), objectKey(env, dataplaneName), ds)))
	require.NoError(t, env.Client.Delete(t.Context(), secret))
	plan, result, err := planAt(t.Context(), env, now.Add(10*caRotationInterval))
	require.NoError(t, err)
	require.Zero(t, plan.Len())
	require.Zero(t, result.RequeueAfter)
	require.True(t, apierrors.IsNotFound(env.Client.Get(t.Context(), objectKey(env, tlsName), secret)))
}

func TestRetainedTLSOwnershipAndFailureBoundaries(t *testing.T) {
	for _, scenario := range []string{"no-claim", "reserved", "fresh", "wrong-claim", "wrong-marker", "missing-marker", "bad-marker-state", "bad-secret", "read-failure"} {
		t.Run(scenario, func(t *testing.T) {
			env := testEnv(t, volume("cache"))
			initialize(t, env)
			require.NoError(t, env.Client.Delete(t.Context(), volume("cache")))

			secret := &corev1.Secret{}
			require.NoError(t, env.Client.Get(t.Context(), objectKey(env, tlsName), secret))
			at := stateOf(t, secret).CreatedAt.Add(caRotationInterval)
			claim, marker := &corev1.ConfigMap{}, &corev1.ConfigMap{}
			require.NoError(t, env.Client.Get(t.Context(), objectKey(env, claimName), claim))
			require.NoError(t, env.Client.Get(t.Context(), objectKey(env, markerName), marker))

			switch scenario {
			case "no-claim":
				require.NoError(t, env.Client.Delete(t.Context(), claim))
			case "reserved":
				claim.Data["state"] = "reserved"
				require.NoError(t, env.Client.Update(t.Context(), claim))
			case "fresh":
				marker.Data["state"] = "fresh"
				marker.Immutable = nil
				require.NoError(t, env.Client.Update(t.Context(), marker))
			case "wrong-claim":
				claim.Annotations[managerAnnotation] = "standalone"
				require.NoError(t, env.Client.Update(t.Context(), claim))
			case "wrong-marker":
				marker.Annotations[claimAnnotation] = "foreign"
				require.NoError(t, env.Client.Update(t.Context(), marker))
			case "missing-marker":
				require.NoError(t, env.Client.Delete(t.Context(), marker))
			case "bad-marker-state":
				marker.Data["state"] = "invalid"
				require.NoError(t, env.Client.Update(t.Context(), marker))
			case "bad-secret":
				secret.Data["ca.key"] = []byte("corrupt")
				require.NoError(t, env.Client.Update(t.Context(), secret))
			case "read-failure":
				env.APIReader = interceptor.NewClient(env.Client.(client.WithWatch), interceptor.Funcs{
					Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
						return errors.New("API unavailable")
					},
				})
			}

			plan, _, err := planAt(t.Context(), env, at)
			if scenario == "no-claim" || scenario == "reserved" || scenario == "fresh" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}

			require.Zero(t, plan.Len())
		})
	}
}

func TestRetainedTrustRepair(t *testing.T) {
	for _, scenario := range []string{"missing", "changed", "deleting", "read-failure"} {
		t.Run(scenario, func(t *testing.T) {
			env := testEnv(t, volume("cache"))
			initialize(t, env)
			require.NoError(t, env.Client.Delete(t.Context(), volume("cache")))

			secret := &corev1.Secret{}
			require.NoError(t, env.Client.Get(t.Context(), objectKey(env, tlsName), secret))
			at := stateOf(t, secret).CreatedAt
			trust := &corev1.ConfigMap{}
			require.NoError(t, env.Client.Get(t.Context(), objectKey(env, trustName), trust))

			switch scenario {
			case "missing":
				require.NoError(t, env.Client.Delete(t.Context(), trust))
			case "changed":
				trust.Data["ca.crt"] = "stale"
				trust.Data["admin"] = "preserve"
				require.NoError(t, env.Client.Update(t.Context(), trust))
			case "deleting":
				trust.Finalizers = []string{"test"}
				require.NoError(t, env.Client.Update(t.Context(), trust))
				require.NoError(t, env.Client.Delete(t.Context(), trust))
			case "read-failure":
				env.APIReader = interceptor.NewClient(env.Client.(client.WithWatch), interceptor.Funcs{
					Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
						if key.Name == trustName {
							return errors.New("trust unavailable")
						}

						return c.Get(ctx, key, obj, opts...)
					},
				})
			}

			plan, _, err := planAt(t.Context(), env, at)
			if scenario == "deleting" || scenario == "read-failure" {
				require.Error(t, err)
				require.Zero(t, plan.Len())

				return
			}

			require.NoError(t, err)
			require.Len(t, plan.Operations, 1)
			require.Equal(t, trustName, plan.Operations[0].Object.GetName())
			persist(t, env, plan)
			require.NoError(t, env.Client.Get(t.Context(), objectKey(env, trustName), trust))
			require.Equal(t, string(secret.Data["ca.crt"]), trust.Data["ca.crt"])

			if scenario == "changed" {
				require.Equal(t, "preserve", trust.Data["admin"])
			}

			plan, result, err := planAt(t.Context(), env, at)
			require.NoError(t, err)
			require.Zero(t, plan.Len())
			require.Equal(t, time.Hour, result.RequeueAfter)
		})
	}
}

func TestRetainedTLSDoesNotAdoptStandalone(t *testing.T) {
	secret, err := newTLS("custom-system", tlsEpoch())
	require.NoError(t, err)
	env := testEnv(t, secret, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: markerName, Namespace: "custom-system"}})
	plan, result, err := planAt(t.Context(), env, tlsEpoch().Add(caRotationInterval))
	require.NoError(t, err)
	require.Zero(t, plan.Len())
	require.Zero(t, result.RequeueAfter)
}
