// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	racerv1 "github.com/Azure/unbounded/api/racer/v1alpha1"
	"github.com/Azure/unbounded/internal/racer/members"
	"github.com/Azure/unbounded/internal/racer/wire"
)

func TestClusterCacheTriggersInstallationAndRetainsIdentity(t *testing.T) {
	v := volume("legacy")
	cache := &racerv1.ClusterCache{ObjectMeta: v.ObjectMeta}
	env := testEnv(t, cache)
	listed := false
	env.APIReader = interceptor.NewClient(env.Client.(client.WithWatch), interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*racerv1.ClusterCacheList); ok {
				listed = true

				require.EqualValues(t, 1, (&client.ListOptions{}).ApplyOptions(opts).Limit)
			}

			return c.List(ctx, list, opts...)
		},
	})
	initialize(t, env)
	require.True(t, listed)
	plan := planPass(t, env)
	require.Contains(t, plan.Summary(), "DaemonSet/custom-system/racer-dataplane")
	persist(t, env, plan)
	catalog, err := members.ReadCatalog(t.Context(), env.Client)
	require.NoError(t, err)
	require.Len(t, catalog, 1)
	require.Equal(t, wire.CacheID(cache.UID), catalog[0].ID)

	other := volume("volume")
	require.NoError(t, env.Client.Create(t.Context(), other))
	require.NoError(t, env.Client.Delete(t.Context(), cache))
	require.Contains(t, planPass(t, env).Summary(), "DaemonSet/custom-system/racer-dataplane")
	require.NoError(t, env.Client.Delete(t.Context(), other))
	plan, result, err := (Component{}).Plan(t.Context(), env, nil)
	require.NoError(t, err)
	require.NotContains(t, plan.Summary(), "DaemonSet/")
	require.Contains(t, result.Message, "retained")
}

func TestCatalogActivationReadFailures(t *testing.T) {
	for _, cacheFailure := range []bool{false, true} {
		env := testEnv(t, volume("trigger"))
		cause := errors.New("catalog read failed")
		env.APIReader = interceptor.NewClient(env.Client.(client.WithWatch), interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				_, cache := list.(*racerv1.ClusterCacheList)
				if cache == cacheFailure {
					return cause
				}

				return c.List(ctx, list, opts...)
			},
		})
		plan, _, err := (Component{}).Plan(t.Context(), env, nil)
		require.ErrorIs(t, err, cause)
		require.Nil(t, plan)
	}
}

func TestMixedCatalogCollisionDoesNotChooseAnIdentity(t *testing.T) {
	v := volume("collision")
	cache := &racerv1.ClusterCache{ObjectMeta: metav1.ObjectMeta{Name: v.Name, UID: volume("uid").UID}}
	env := testEnv(t, v, cache)
	require.Positive(t, planPass(t, env).Len(), "activation must not hide invalid catalog resources")
	catalog, err := members.ReadCatalog(t.Context(), env.LiveReader())
	require.ErrorIs(t, err, wire.InvalidRequest)
	require.Nil(t, catalog)
}
