// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	manifests "github.com/Azure/unbounded/deploy/racer"
	"github.com/Azure/unbounded/internal/operator/component"
	racercore "github.com/Azure/unbounded/internal/racer"
)

func identityPlan(t *testing.T, env *component.Env) (*component.Plan, *corev1.ConfigMap) {
	t.Helper()

	plan := component.NewPlan()
	marker, err := planIdentity(t.Context(), env, plan)
	require.NoError(t, err)

	return plan, marker
}

func finishIdentity(t *testing.T, env *component.Env) *corev1.ConfigMap {
	t.Helper()

	for range 6 {
		plan, marker := identityPlan(t, env)
		if marker != nil {
			require.Equal(t, "fresh", marker.Data["state"])
			return marker
		}

		persist(t, env, plan)
	}

	t.Fatal("identity did not converge")

	return nil
}

func TestIdentityInterruptionBoundaries(t *testing.T) {
	for phase := range 4 {
		for _, boundary := range []string{"before", "after", "cancel"} {
			t.Run(fmt.Sprintf("%d/%s", phase, boundary), func(t *testing.T) {
				env := testEnv(t)
				for range phase {
					plan, _ := identityPlan(t, env)
					persist(t, env, plan)
				}

				plan, marker := identityPlan(t, env)
				require.Nil(t, marker)
				require.Len(t, plan.Operations, 1)

				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()

				boom := errors.New("interrupted")
				write := func(f func() error) error {
					if boundary == "before" {
						return boom
					}

					if err := f(); err != nil {
						return err
					}

					if boundary == "cancel" {
						cancel()
						return ctx.Err()
					}

					return boom
				}
				executor := *env
				executor.Client = interceptor.NewClient(env.Client.(client.WithWatch), interceptor.Funcs{
					Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
						return write(func() error { return c.Create(ctx, obj, opts...) })
					},
					Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
						return write(func() error { return c.Patch(ctx, obj, patch, opts...) })
					},
				})
				result, err := executor.Execute(ctx, plan)
				require.NoError(t, err)
				require.Error(t, result.Err())

				before := &corev1.ConfigMap{}
				err = env.Client.Get(t.Context(), objectKey(env, markerName), before)
				require.True(t, err == nil || apierrors.IsNotFound(err))

				if err == nil && before.Data["state"] == operatorPending {
					cfg, err := racercore.ConfigFromLookup(func(key string) (string, bool) {
						switch key {
						case "RACER_CLUSTER_ID":
							return before.Data["cluster"], true
						case "POD_NAMESPACE":
							return env.Namespace, true
						}

						return "", false
					})
					require.NoError(t, err)
					require.Error(t, racercore.Assemble(cfg, env.Client, env.APIReader).Recover(t.Context(), env.Client))
					require.True(t, apierrors.IsNotFound(env.Client.Get(t.Context(), objectKey(env, versionName), &corev1.ConfigMap{})))
				}

				after := finishIdentity(t, env)
				if before.UID != "" {
					require.Equal(t, before.UID, after.UID)
				}

				claim := &corev1.ConfigMap{}
				require.NoError(t, env.Client.Get(t.Context(), objectKey(env, claimName), claim))
				require.True(t, *claim.Immutable)
				require.Equal(t, string(after.UID), claim.Data[operatorMarkerUID])
			})
		}
	}
}

func TestIdentityCompetingAndStalePlans(t *testing.T) {
	env := testEnv(t)
	for range 4 {
		first, _ := identityPlan(t, env)
		second, _ := identityPlan(t, env)
		persist(t, env, first)
		result, err := env.Execute(t.Context(), second)
		require.NoError(t, err)
		require.NoError(t, result.Err())
	}

	finishIdentity(t, env)

	for _, stalePhase := range []int{1, 3} {
		t.Run(fmt.Sprint(stalePhase), func(t *testing.T) {
			env := testEnv(t)
			for range stalePhase {
				plan, _ := identityPlan(t, env)
				persist(t, env, plan)
			}

			stale, _ := identityPlan(t, env)
			marker := finishIdentity(t, env)
			require.NoError(t, env.Client.Delete(t.Context(), marker))

			if stalePhase == 3 {
				marker.ResourceVersion = ""
				marker.Data["state"] = operatorPending
				require.NoError(t, env.Client.Create(t.Context(), marker))
				// Fake API revisions can repeat after deletion; advance for CAS.
				require.NoError(t, env.Client.Update(t.Context(), marker))
			}

			result, err := env.Execute(t.Context(), stale)
			require.NoError(t, err)

			if stalePhase == 3 {
				require.Len(t, result.Deferred, 1)
			}

			require.NoError(t, env.Client.Get(t.Context(), objectKey(env, markerName), marker))
			require.Equal(t, operatorPending, marker.Data["state"])

			plan := component.NewPlan()
			_, err = planIdentity(t.Context(), env, plan)
			require.Error(t, err)
			require.Zero(t, plan.Len())
		})
	}
}

func TestIdentityRejectsCorruptCandidates(t *testing.T) {
	for _, field := range []string{"cluster", "state", operatorInitialization, "binding", "version"} {
		t.Run(field, func(t *testing.T) {
			env := testEnv(t)
			for range 2 {
				plan, _ := identityPlan(t, env)
				persist(t, env, plan)
			}

			marker := &corev1.ConfigMap{}
			require.NoError(t, env.Client.Get(t.Context(), objectKey(env, markerName), marker))

			switch field {
			case "binding":
				marker.Annotations[claimAnnotation] = "wrong"
			case "version":
				require.NoError(t, env.Client.Create(t.Context(), &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: versionName, Namespace: env.Namespace}}))
			default:
				marker.Data[field] = "wrong"
			}

			require.NoError(t, env.Client.Update(t.Context(), marker))

			plan := component.NewPlan()
			_, err := planIdentity(t.Context(), env, plan)
			require.Error(t, err)
			require.Zero(t, plan.Len())
		})
	}
}

func TestIdentityRejectsManifestCollisions(t *testing.T) {
	objects, err := testEnv(t).DecodeManifestFS(manifests.Manifests, nil)
	require.NoError(t, err)
	require.NotEmpty(t, objects)

	for _, obj := range objects {
		for _, phase := range []string{"absent", "reserved", "staged", "legacy"} {
			t.Run(obj.GetKind()+"/"+obj.GetName()+"/"+phase, func(t *testing.T) {
				env := testEnv(t, cacheObject("cache"))
				if phase != "absent" {
					plan, _ := identityPlan(t, env)
					persist(t, env, plan)
				}

				if phase == "staged" {
					plan, _ := identityPlan(t, env)
					persist(t, env, plan)
				}

				if phase == "legacy" {
					claim := &corev1.ConfigMap{}
					require.NoError(t, env.Client.Get(t.Context(), objectKey(env, claimName), claim))
					delete(claim.Data, operatorInitialization)
					require.NoError(t, env.Client.Update(t.Context(), claim))
				}

				existing := obj.DeepCopy()
				require.NoError(t, env.Client.Create(t.Context(), existing))
				before := existing.DeepCopy()
				require.NoError(t, env.Client.Get(t.Context(), client.ObjectKeyFromObject(existing), before))

				plan, _, err := (Component{}).Plan(t.Context(), env, nil)
				require.ErrorContains(t, err, "standalone installations are not adopted")
				require.Nil(t, plan)
				require.NoError(t, env.Client.Get(t.Context(), client.ObjectKeyFromObject(existing), existing))
				require.Equal(t, before, existing)
			})
		}
	}
}

func TestIdentityInventoryScope(t *testing.T) {
	env := testEnv(t)
	objects, err := env.DecodeManifestFS(manifests.Manifests, nil)
	require.NoError(t, err)

	for _, obj := range objects {
		if obj.GetNamespace() != "" {
			obj.SetNamespace("other-system")
		} else {
			obj.SetName(obj.GetName() + "-other")
		}

		require.NoError(t, env.Client.Create(t.Context(), obj))
	}

	finishIdentity(t, env)
}

func TestIdentityInventoryReadFailure(t *testing.T) {
	env := testEnv(t, cacheObject("cache"))
	boom := errors.New("inventory unavailable")
	env.APIReader = interceptor.NewClient(env.Client.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if obj.GetObjectKind().GroupVersionKind().Kind == "ServiceAccount" && key == objectKey(env, controllerName) {
				return boom
			}

			return c.Get(ctx, key, obj, opts...)
		},
	})
	plan, _, err := (Component{}).Plan(t.Context(), env, nil)
	require.ErrorIs(t, err, boom)
	require.Nil(t, plan)
}
