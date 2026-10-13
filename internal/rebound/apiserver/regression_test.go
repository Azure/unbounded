// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package apiserver

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metainternal "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	clienttesting "k8s.io/client-go/testing"
	clocktesting "k8s.io/utils/clock/testing"

	rebound "github.com/Azure/unbounded/api/rebound/v1alpha1"
	"github.com/Azure/unbounded/internal/rebound/ledger"
)

func TestImmediateReadAfterCreate(t *testing.T) {
	s, client, ctx := fixture(t)
	created, err := s.Create(ctx, grant("first", "node-a"), nil, nil)
	require.NoError(t, err)

	got, err := s.Get(ctx, "first", nil)
	require.NoError(t, err)
	require.Equal(t, created, got)

	listed, err := s.List(ctx, nil)
	require.NoError(t, err)

	list, ok := listed.(*rebound.NodeGrantList)
	require.True(t, ok)
	require.Len(t, list.Items, 1)
	require.Equal(t, got, &list.Items[0])
	require.Equal(t, "2", list.ResourceVersion)

	client.ClearActions()

	_, err = s.Get(ctx, "first", &metav1.GetOptions{ResourceVersion: "0"})
	require.True(t, apierrors.IsNotFound(err), "%v", err)
	listed, err = s.List(ctx, &metainternal.ListOptions{ResourceVersion: "0"})
	require.NoError(t, err)

	list, ok = listed.(*rebound.NodeGrantList)
	require.True(t, ok)
	require.Empty(t, list.Items)
	require.Equal(t, "1", list.ResourceVersion)
	require.Empty(t, client.Actions())

	for _, rv := range []string{"", "0", "2", "unknown"} {
		_, err = s.List(ctx, &metainternal.ListOptions{ResourceVersion: rv, ResourceVersionMatch: metav1.ResourceVersionMatchExact})
		if rv == "unknown" {
			require.True(t, apierrors.IsBadRequest(err), "RV %q: %v", rv, err)
		} else {
			require.True(t, apierrors.IsResourceExpired(err), "RV %q: %v", rv, err)
		}
	}

	_, err = s.List(ctx, &metainternal.ListOptions{ResourceVersion: "1", ResourceVersionMatch: metav1.ResourceVersionMatchExact})
	require.NoError(t, err)
	cached(t, s)
	_, err = s.List(ctx, &metainternal.ListOptions{ResourceVersion: "2", ResourceVersionMatch: metav1.ResourceVersionMatchExact})
	require.NoError(t, err)
}

func TestLiveReadErrorsAndMissingLedger(t *testing.T) {
	s, client, ctx := fixture(t)
	require.NoError(t, client.CoreV1().ConfigMaps("system").Delete(ctx, ledger.ConfigMapName, metav1.DeleteOptions{}))
	_, err := s.Get(ctx, "missing", nil)
	require.True(t, apierrors.IsNotFound(err), "%v", err)
	obj, err := s.List(ctx, nil)
	require.NoError(t, err)

	list, ok := obj.(*rebound.NodeGrantList)
	require.True(t, ok)
	require.Empty(t, list.Items)

	client.PrependReactor("get", "configmaps", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewServiceUnavailable("ledger unavailable")
	})

	_, err = s.Get(ctx, "missing", nil)
	require.True(t, apierrors.IsServiceUnavailable(err), "%v", err)
	_, err = s.List(ctx, nil)
	require.True(t, apierrors.IsServiceUnavailable(err), "%v", err)
	_, err = s.List(ctx, &metainternal.ListOptions{ResourceVersion: "0"})
	require.NoError(t, err)
}

func TestCreateReapsExpired(t *testing.T) {
	for _, name := range []string{"first", "replacement"} {
		t.Run(name, func(t *testing.T) {
			s, _, ctx := fixture(t)
			old := created(t, s, ctx, "first", "node-a")
			c, ok := s.clock.(*clocktesting.FakeClock)
			require.True(t, ok)
			c.Step(time.Hour)

			before := cached(t, s).Data

			_, err := s.Create(ctx, grant(name, "node-a"), nil, &metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}})
			require.NoError(t, err)
			require.Equal(t, before, cached(t, s).Data)

			_, err = s.Create(ctx, grant(name, "missing"), nil, nil)
			require.True(t, apierrors.IsInvalid(err), "%v", err)
			require.Equal(t, before, cached(t, s).Data)

			fresh := created(t, s, ctx, name, "node-a")
			require.NotEqual(t, old.UID, fresh.UID)
			grants := ledger.Parse(cached(t, s).Data).Grants()
			require.Len(t, grants, 1)
			require.Equal(t, name, grants[0].Name)
		})
	}
}

func TestDeleteExpired(t *testing.T) {
	s, _, ctx := fixture(t)
	g := created(t, s, ctx, "first", "node-a")
	c, ok := s.clock.(*clocktesting.FakeClock)
	require.True(t, ok)
	c.Step(time.Hour)

	_, err := s.Get(ctx, "first", nil)
	require.True(t, apierrors.IsNotFound(err), "%v", err)
	_, _, err = s.Delete(ctx, "first", nil, &metav1.DeleteOptions{DryRun: []string{metav1.DryRunAll}})
	require.NoError(t, err)
	require.Len(t, ledger.Parse(cached(t, s).Data).Grants(), 1)

	obj, deleted, err := s.Delete(ctx, "first", nil, &metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &g.UID}})
	require.NoError(t, err)
	require.True(t, deleted)

	got, ok := obj.(*rebound.NodeGrant)
	require.True(t, ok)
	require.Equal(t, g.UID, got.UID)

	l := ledger.Parse(cached(t, s).Data)
	require.Empty(t, l.Grants())
	require.Len(t, l.Releases(), 1)
	require.Equal(t, g.UID, l.Releases()[0].UID)
}

func TestCreateChecksNodesOnRetry(t *testing.T) {
	s, client, ctx := fixture(t)
	client.ClearActions()

	attempts := 0

	client.PrependReactor("update", "configmaps", func(clienttesting.Action) (bool, runtime.Object, error) {
		attempts++
		if attempts == 1 {
			require.NoError(t, client.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("nodes"), "", "node-a"))
			return true, nil, apierrors.NewConflict(corev1.Resource("configmaps"), ledger.ConfigMapName, errors.New("retry"))
		}

		return false, nil, nil
	})

	_, err := s.Create(ctx, grant("first", "node-a"), nil, nil)
	require.True(t, apierrors.IsInvalid(err), "%v", err)
	status, ok := err.(apierrors.APIStatus)
	require.True(t, ok)
	require.Equal(t, int32(422), status.Status().Code)
	require.Equal(t, 1, attempts)

	var actions []string
	for _, action := range client.Actions() {
		actions = append(actions, action.GetVerb()+" "+action.GetResource().Resource)
	}

	require.Equal(t, []string{"get namespaces", "get configmaps", "get nodes", "update configmaps", "get configmaps", "get nodes"}, actions)
	require.Empty(t, cached(t, s).Data)
}

func TestWatchResumeWithinDeletionBatch(t *testing.T) {
	for _, replay := range []bool{false, true} {
		t.Run(fmt.Sprint("replay=", replay), func(t *testing.T) {
			s, _, ctx := fixture(t)
			created(t, s, ctx, "first", "node-a")
			created(t, s, ctx, "second", "node-b")

			options := &metainternal.ListOptions{ResourceVersion: "3"}
			w, err := s.Watch(ctx, options)
			require.NoError(t, err)

			defer w.Stop()

			_, err = s.DeleteCollection(ctx, nil, nil, nil)
			require.NoError(t, err)
			cm := cached(t, s)

			if replay {
				w.Stop()
				w, err = s.Watch(ctx, options)
				require.NoError(t, err)

				defer w.Stop()
			}

			var first watch.Event
			select {
			case first = <-w.ResultChan():
			case <-time.After(5 * time.Second):
				t.Fatal("deletion event timed out")
			}

			g, ok := first.Object.(*rebound.NodeGrant)
			require.True(t, ok)
			require.Equal(t, watch.Deleted, first.Type)
			require.Equal(t, "first", g.Name)
			require.Equal(t, "3", g.ResourceVersion)
			w.Stop()

			resumed, err := s.Watch(ctx, &metainternal.ListOptions{ResourceVersion: g.ResourceVersion})
			require.NoError(t, err)

			defer resumed.Stop()

			event(t, resumed, watch.Deleted, "first", "3")
			event(t, resumed, watch.Deleted, "second", "4")

			for i := 5; i < historyLimit+5; i++ {
				cm.ResourceVersion = fmt.Sprint(i)
				s.observe(cm, false)
			}

			_, err = s.Watch(ctx, &metainternal.ListOptions{ResourceVersion: g.ResourceVersion})
			require.True(t, apierrors.IsResourceExpired(err), "%v", err)
		})
	}
}
