// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

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

func testOperatorIdentityBoundaries(t *testing.T, makeEnv func(*testing.T) *component.Env) {
	t.Helper()

	for phase := range 4 { // claim Create, marker Create, claim CAS, promotion CAS
		for _, boundary := range []string{"before", "after", "cancel"} {
			t.Run(fmt.Sprintf("phase=%d/%s", phase, boundary), func(t *testing.T) {
				env := makeEnv(t)
				for range phase {
					plan, marker := identityPlan(t, env)
					require.Nil(t, marker)
					persist(t, env, plan)
				}

				plan, marker := identityPlan(t, env)
				require.Nil(t, marker)
				require.Len(t, plan.Operations, 1)

				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()

				boom := errors.New("interrupted operator initialization")
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
				writer := interceptor.NewClient(env.Client.(client.WithWatch), interceptor.Funcs{
					Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
						return write(func() error { return c.Create(ctx, obj, opts...) })
					},
					Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
						return write(func() error { return c.Patch(ctx, obj, patch, opts...) })
					},
				})
				executor := *env
				executor.Client = writer
				result, err := executor.Execute(ctx, plan)
				require.NoError(t, err)
				require.Error(t, result.Err())

				before := &corev1.ConfigMap{}
				err = env.Client.Get(t.Context(), objectKey(env, markerName), before)
				require.True(t, err == nil || apierrors.IsNotFound(err))

				if err == nil && before.Data["state"] == operatorPending {
					// Even a separately started controller cannot initialize a staged
					// marker before its UID-bound promotion.
					t.Setenv("RACER_CLUSTER_ID", before.Data["cluster"])
					t.Setenv("POD_NAMESPACE", env.Namespace)

					cfg, err := racercore.LoadConfig()
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
				require.NotNil(t, claim.Immutable)
				require.True(t, *claim.Immutable)
				require.Equal(t, string(after.UID), claim.Data[operatorMarkerUID])
				require.Equal(t, string(claim.UID), after.Annotations[claimAnnotation])
			})
		}
	}
}

func TestOperatorStagedIdentityBoundaries(t *testing.T) {
	testOperatorIdentityBoundaries(t, func(t *testing.T) *component.Env { return testEnv(t) })
}

func TestOperatorStagedIdentityLostCommittedMarker(t *testing.T) {
	for _, promoted := range []bool{false, true} {
		for _, replace := range []bool{false, true} {
			t.Run(fmt.Sprintf("promoted=%t/replace=%t", promoted, replace), func(t *testing.T) {
				env := testEnv(t)
				for range 3 {
					plan, _ := identityPlan(t, env)
					persist(t, env, plan)
				}

				if promoted {
					finishIdentity(t, env)
				}

				marker := &corev1.ConfigMap{}
				require.NoError(t, env.Client.Get(t.Context(), objectKey(env, markerName), marker))
				require.NoError(t, env.Client.Delete(t.Context(), marker))

				if replace {
					marker.ResourceVersion = ""
					require.NoError(t, env.Client.Create(t.Context(), marker))
				}

				plan := component.NewPlan()
				_, err := planIdentity(t.Context(), env, plan)
				require.Error(t, err)
				require.Zero(t, plan.Len())
			})
		}
	}
}

func TestOperatorStagedIdentityDelayedCreate(t *testing.T) {
	env := testEnv(t)
	plan, _ := identityPlan(t, env)
	persist(t, env, plan)
	stale, _ := identityPlan(t, env)
	marker := finishIdentity(t, env)
	require.NoError(t, env.Client.Delete(t.Context(), marker))
	// A stale plan can Create only operator-pending, never an active marker.
	persist(t, env, stale)
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, markerName), marker))
	require.Equal(t, operatorPending, marker.Data["state"])

	plan = component.NewPlan()
	_, err := planIdentity(t.Context(), env, plan)
	require.Error(t, err)
	require.Zero(t, plan.Len())
}

func TestOperatorStagedIdentityDelayedPromotion(t *testing.T) {
	env := testEnv(t)
	for range 3 {
		plan, _ := identityPlan(t, env)
		persist(t, env, plan)
	}

	stale, _ := identityPlan(t, env)
	marker := &corev1.ConfigMap{}
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, markerName), marker))
	require.NoError(t, env.Client.Delete(t.Context(), marker))
	marker.ResourceVersion = ""
	require.NoError(t, env.Client.Create(t.Context(), marker))
	// The fake client can reuse resource versions after deletion. Use a newer
	// version to model the real API's never-reused etcd modification revision.
	require.NoError(t, env.Client.Update(t.Context(), marker))
	result, err := env.Execute(t.Context(), stale)
	require.NoError(t, err)
	require.Len(t, result.Deferred, 1)
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, markerName), marker))
	require.Equal(t, operatorPending, marker.Data["state"])

	plan := component.NewPlan()
	_, err = planIdentity(t.Context(), env, plan)
	require.Error(t, err)
	require.Zero(t, plan.Len())
}

func TestOperatorStagedIdentityCompetingPlans(t *testing.T) {
	env := testEnv(t)
	for range 4 {
		first, _ := identityPlan(t, env)
		second, _ := identityPlan(t, env)
		persist(t, env, first)
		result, err := env.Execute(t.Context(), second)
		require.NoError(t, err)
		require.NoError(t, result.Err()) // conflicts defer for replanning
	}

	finishIdentity(t, env)
}

func TestOperatorStagedIdentityRejectsCandidates(t *testing.T) {
	for _, corruption := range []string{"binding", "cluster", "state", "protocol", "immutable", "version"} {
		t.Run(corruption, func(t *testing.T) {
			env := testEnv(t)
			for range 2 {
				plan, _ := identityPlan(t, env)
				persist(t, env, plan)
			}

			marker := &corev1.ConfigMap{}
			require.NoError(t, env.Client.Get(t.Context(), objectKey(env, markerName), marker))

			switch corruption {
			case "binding":
				marker.Annotations[claimAnnotation] = "foreign"
			case "cluster":
				marker.Data["cluster"] = "foreign"
			case "state":
				marker.Data["state"] = "fresh"
			case "protocol":
				delete(marker.Data, operatorInitialization)
			case "immutable":
				immutable := true
				marker.Immutable = &immutable
			case "version":
				require.NoError(t, env.Client.Create(t.Context(), &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: versionName, Namespace: env.Namespace}}))
			}

			require.NoError(t, env.Client.Update(t.Context(), marker))

			plan := component.NewPlan()
			_, err := planIdentity(t.Context(), env, plan)
			require.Error(t, err)
			require.Zero(t, plan.Len())
		})
	}
}

func TestOperatorIdentityRecoveryEnvtest(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("set KUBEBUILDER_ASSETS for real API-server identity recovery")
	}

	environment := &envtest.Environment{BinaryAssetsDirectory: assets}
	rc, err := environment.Start()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, environment.Stop()) })

	c, err := client.NewWithWatch(rc, client.Options{Scheme: clientgoscheme.Scheme})
	require.NoError(t, err)

	index := 0

	testOperatorIdentityBoundaries(t, func(t *testing.T) *component.Env {
		index++
		namespace := fmt.Sprintf("operator-identity-%d", index)
		require.NoError(t, c.Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}))

		return &component.Env{Client: c, APIReader: c, Namespace: namespace, Scheme: clientgoscheme.Scheme}
	})
	t.Run("immutable-commitment", func(t *testing.T) {
		namespace := "operator-immutable-commitment"
		require.NoError(t, c.Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}))
		env := &component.Env{Client: c, APIReader: c, Namespace: namespace, Scheme: clientgoscheme.Scheme}
		finishIdentity(t, env)

		claim := &corev1.ConfigMap{}
		require.NoError(t, c.Get(t.Context(), objectKey(env, claimName), claim))
		claim.Data[operatorMarkerUID] = "replacement"
		require.Error(t, c.Update(t.Context(), claim))
	})

	t.Run("stale-promotion-after-replacement", func(t *testing.T) {
		namespace := "operator-stale-promotion"
		require.NoError(t, c.Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}))

		env := &component.Env{Client: c, APIReader: c, Namespace: namespace, Scheme: clientgoscheme.Scheme}
		for range 3 {
			plan, _ := identityPlan(t, env)
			persist(t, env, plan)
		}

		stale, _ := identityPlan(t, env)
		marker := &corev1.ConfigMap{}
		require.NoError(t, c.Get(t.Context(), objectKey(env, markerName), marker))
		require.NoError(t, c.Delete(t.Context(), marker))
		marker.ResourceVersion, marker.UID = "", ""
		require.NoError(t, c.Create(t.Context(), marker))
		result, err := env.Execute(t.Context(), stale)
		require.NoError(t, err)
		require.Len(t, result.Deferred, 1)
		require.NoError(t, c.Get(t.Context(), objectKey(env, markerName), marker))
		require.Equal(t, operatorPending, marker.Data["state"])

		plan := component.NewPlan()
		_, err = planIdentity(t.Context(), env, plan)
		require.Error(t, err)
		require.Zero(t, plan.Len())
	})
}
