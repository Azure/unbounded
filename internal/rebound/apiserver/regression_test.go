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
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
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
			// Expiry starts the handoff cooldown. Step past both.
			c.Step(time.Hour + ledger.HandoffCooldown)

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

	// The expired grant is still in the ledger, so it is still readable.
	_, err := s.Get(ctx, "first", nil)
	require.NoError(t, err)
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

// TestExpiredGrantVisibleUntilReaped checks that reads depend only on the
// ledger snapshot. The clock passing expiresAt changes neither the RV nor the
// watch stream, so it must not change what get, list, or a new watch return.
func TestExpiredGrantVisibleUntilReaped(t *testing.T) {
	s, _, ctx := fixture(t)
	c, ok := s.clock.(*clocktesting.FakeClock)
	require.True(t, ok)

	short := grant("first", "node-a")
	short.Spec.TTL = &metav1.Duration{Duration: time.Minute}
	_, err := s.Create(ctx, short, nil, nil)
	require.NoError(t, err)

	rv := cached(t, s).ResourceVersion

	listed, err := s.List(ctx, &metainternal.ListOptions{ResourceVersion: rv, ResourceVersionMatch: metav1.ResourceVersionMatchExact})
	require.NoError(t, err)

	before, ok := listed.(*rebound.NodeGrantList)
	require.True(t, ok)
	require.Equal(t, rv, before.ResourceVersion)
	require.Len(t, before.Items, 1)

	informer, err := s.Watch(ctx, &metainternal.ListOptions{ResourceVersion: rv})
	require.NoError(t, err)
	t.Cleanup(informer.Stop)

	// Expire the grant by the clock alone. Nothing writes the ledger.
	c.Step(2 * time.Minute)
	require.True(t, before.Items[0].Status.ExpiresAt.Time.Before(c.Now()))

	for _, getRV := range []string{"", "0", rv} {
		obj, err := s.Get(ctx, "first", &metav1.GetOptions{ResourceVersion: getRV})
		require.NoError(t, err, "RV %q", getRV)
		require.Equal(t, &before.Items[0], obj, "RV %q", getRV)
	}

	for _, options := range []*metainternal.ListOptions{
		nil,
		{ResourceVersion: "0"},
		{ResourceVersion: rv},
		{ResourceVersion: rv, ResourceVersionMatch: metav1.ResourceVersionMatchExact},
	} {
		got, err := s.List(ctx, options)
		require.NoError(t, err)
		require.Equal(t, before, got, "options %+v", options)
	}

	prefill, err := s.Watch(ctx, nil)
	require.NoError(t, err)
	t.Cleanup(prefill.Stop)
	event(t, prefill, watch.Added, "first", rv)

	// A past expiry renders as its timestamp in the table.
	table, err := s.ConvertToTable(ctx, &before.Items[0], nil)
	require.NoError(t, err)
	require.Len(t, table.Rows, 1)
	require.Equal(t, before.Items[0].Status.ExpiresAt.UTC().Format(time.RFC3339), table.Rows[0].Cells[3])

	// The reaper's ledger write removes the grant and produces the event.
	_, _, err = s.store.Update(ctx, false, func(l *ledger.Ledger) error {
		removed, _ := l.Reap(c.Now(), nil)
		require.Len(t, removed, 1)

		return nil
	})
	require.NoError(t, err)

	reaped := cached(t, s).ResourceVersion
	require.NotEqual(t, rv, reaped)
	event(t, informer, watch.Deleted, "first", reaped)
	event(t, prefill, watch.Deleted, "first", reaped)

	_, err = s.Get(ctx, "first", nil)
	require.True(t, apierrors.IsNotFound(err), "%v", err)

	listed, err = s.List(ctx, &metainternal.ListOptions{ResourceVersion: "0"})
	require.NoError(t, err)

	after, ok := listed.(*rebound.NodeGrantList)
	require.True(t, ok)
	require.Equal(t, reaped, after.ResourceVersion)
	require.Empty(t, after.Items)
}

func TestCreateChecksNamespaceOnRetry(t *testing.T) {
	for _, recreate := range []bool{true, false} {
		t.Run(fmt.Sprint("recreate=", recreate), func(t *testing.T) {
			s, client, ctx := fixture(t)
			attempts := 0
			namespaceGets := 0

			client.PrependReactor("get", "namespaces", func(clienttesting.Action) (bool, runtime.Object, error) {
				namespaceGets++
				return false, nil, nil
			})
			client.PrependReactor("update", "configmaps", func(clienttesting.Action) (bool, runtime.Object, error) {
				attempts++
				if attempts != 1 {
					return false, nil, nil
				}

				resource := corev1.SchemeGroupVersion.WithResource("namespaces")
				require.NoError(t, client.Tracker().Delete(resource, "", "team"))

				if recreate {
					require.NoError(t, client.Tracker().Create(resource, &corev1.Namespace{
						ObjectMeta: metav1.ObjectMeta{Name: "team", UID: "new-team-uid"},
					}, ""))
				}

				return true, nil, apierrors.NewConflict(corev1.Resource("configmaps"), ledger.ConfigMapName, errors.New("retry"))
			})

			_, err := s.Create(ctx, grant("first", "node-a"), nil, nil)

			require.Equal(t, 2, namespaceGets)

			if recreate {
				require.NoError(t, err)
				require.Equal(t, 2, attempts)
				stored, ok := ledger.Parse(cached(t, s).Data).Get(types.NamespacedName{Namespace: "team", Name: "first"})
				require.True(t, ok)
				require.Equal(t, types.UID("new-team-uid"), stored.NamespaceUID)
			} else {
				require.True(t, apierrors.IsInvalid(err), "%v", err)
				status, ok := err.(apierrors.APIStatus)
				require.True(t, ok)
				require.Equal(t, int32(422), status.Status().Code)
				require.Len(t, status.Status().Details.Causes, 1)
				require.Equal(t, "metadata.namespace", status.Status().Details.Causes[0].Field)
				require.Contains(t, status.Status().Details.Causes[0].Message, "namespace does not exist")
				require.Equal(t, 1, attempts)
				require.Empty(t, cached(t, s).Data)
			}
		})
	}
}

func TestCreateNodeLimit(t *testing.T) {
	for _, limit := range []int{1, 500} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			s, client, ctx := fixture(t)
			s.maxNodesPerGrant = limit

			nodes := make([]string, limit+1)
			items := make([]corev1.Node, limit+1)

			for i := range nodes {
				nodes[i] = fmt.Sprintf("node-%d", i)
				items[i] = corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodes[i], Labels: map[string]string{"kubernetes.io/hostname": nodes[i]}}}
			}

			calls := 0

			client.PrependReactor("list", "nodes", func(action clienttesting.Action) (bool, runtime.Object, error) {
				calls++
				list, ok := action.(clienttesting.ListActionImpl)
				require.True(t, ok)
				require.Equal(t, int64(limit)+1, list.GetListOptions().Limit)

				sel, err := metav1.LabelSelectorAsSelector(grant("too-many", nodes...).Spec.NodeSelector)
				require.NoError(t, err)
				require.Equal(t, sel.String(), list.GetListOptions().LabelSelector)

				return true, &corev1.NodeList{Items: items}, nil
			})

			client.ClearActions()

			_, err := s.Create(ctx, grant("too-many", nodes...), nil, nil)
			require.True(t, apierrors.IsInvalid(err), "%v", err)
			status, ok := err.(apierrors.APIStatus)
			require.True(t, ok)
			require.Equal(t, int32(422), status.Status().Code)
			require.Len(t, status.Status().Details.Causes, 1)
			require.Equal(t, metav1.CauseTypeTooMany, status.Status().Details.Causes[0].Type)
			require.Equal(t, "spec.nodeSelector", status.Status().Details.Causes[0].Field)
			require.Equal(t, 1, calls)
			require.Empty(t, cached(t, s).Data)
		})
	}
}

func TestCreateListsNodesOnce(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(fmt.Sprint("missing=", missing), func(t *testing.T) {
			s, client, ctx := fixture(t)
			s.maxNodesPerGrant = 2

			nodes := []string{"node-a", "node-b"}
			if missing {
				nodes = []string{"missing-a", "missing-b"}
				s.maxNodesPerGrant = len(nodes)
			}

			client.ClearActions()

			_, err := s.Create(ctx, grant("first", nodes...), nil, nil)
			if missing {
				require.True(t, apierrors.IsInvalid(err), "%v", err)
				status, ok := err.(apierrors.APIStatus)
				require.True(t, ok)
				require.Equal(t, int32(422), status.Status().Code)
				causes := status.Status().Details.Causes
				require.Len(t, causes, 1)
				require.Equal(t, metav1.CauseTypeFieldValueInvalid, causes[0].Type)
				require.Equal(t, "spec.nodeSelector", causes[0].Field)
				require.Contains(t, causes[0].Message, "matches no nodes")
			} else {
				require.NoError(t, err, "a grant at the limit must succeed")
			}

			var nodeActions []string

			for _, action := range client.Actions() {
				if action.GetResource().Resource == "nodes" {
					nodeActions = append(nodeActions, action.GetVerb())
				}
			}

			require.Equal(t, []string{"list"}, nodeActions)

			if missing {
				require.Empty(t, cached(t, s).Data)
			} else {
				require.Len(t, ledger.Parse(cached(t, s).Data).Grants(), 1)
			}
		})
	}
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

	require.Equal(t, []string{"get configmaps", "get namespaces", "list nodes", "update configmaps", "get configmaps", "get namespaces", "list nodes"}, actions)
	require.Empty(t, cached(t, s).Data)
}

func received(t *testing.T, w watch.Interface, typ watch.EventType, name string) *rebound.NodeGrant {
	t.Helper()

	select {
	case e, ok := <-w.ResultChan():
		require.True(t, ok, "watch closed")
		g, ok := e.Object.(*rebound.NodeGrant)
		require.True(t, ok)
		require.Equal(t, typ, e.Type)
		require.Equal(t, name, g.Name)

		return g
	case <-time.After(5 * time.Second):
		t.Fatal("watch event timed out")
		return nil
	}
}

func TestWatchResumeWithinInitialEvents(t *testing.T) {
	s, _, ctx := fixture(t)
	created(t, s, ctx, "first", "node-a")
	created(t, s, ctx, "second", "node-b")

	w, err := s.Watch(ctx, nil)
	require.NoError(t, err)

	defer w.Stop()

	partial := received(t, w, watch.Added, "first").ResourceVersion
	require.NotEqual(t, "3", partial, "a partial snapshot must not advertise the snapshot RV")
	require.NotEqual(t, "0", partial)
	require.NotEmpty(t, partial)
	event(t, w, watch.Added, "second", "3")
	w.Stop()

	// The client stopped after the first initial event, so it must relist.
	_, err = s.Watch(ctx, &metainternal.ListOptions{ResourceVersion: partial})
	require.True(t, apierrors.IsResourceExpired(err), "%v", err)

	listed, err := s.List(ctx, &metainternal.ListOptions{ResourceVersion: partial})
	require.NoError(t, err)

	list, ok := listed.(*rebound.NodeGrantList)
	require.True(t, ok)
	require.Equal(t, "3", list.ResourceVersion)
	require.Len(t, list.Items, 2)

	// The last event this watcher receives carries the snapshot RV, even
	// when the selector skips later grants.
	filtered, err := s.Watch(ctx, &metainternal.ListOptions{FieldSelector: fields.OneTermEqualSelector("metadata.name", "first")})
	require.NoError(t, err)

	defer filtered.Stop()

	event(t, filtered, watch.Added, "first", "3")
	require.Empty(t, filtered.ResultChan())

	resumed, err := s.Watch(ctx, &metainternal.ListOptions{ResourceVersion: "3"})
	require.NoError(t, err)

	defer resumed.Stop()

	require.Empty(t, resumed.ResultChan())

	_, _, err = s.Delete(ctx, "first", nil, nil)
	require.NoError(t, err)
	// The release confirmation write follows the delete.
	cached(t, s)
	event(t, resumed, watch.Deleted, "first", "5")
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

			// The release confirmation write follows the deletes, so the
			// batch ends one version past the removal.
			event(t, resumed, watch.Deleted, "first", "3")
			event(t, resumed, watch.Deleted, "second", "5")

			for i := 6; i < historyLimit+6; i++ {
				cm.ResourceVersion = fmt.Sprint(i)
				s.observe(cm, false)
			}

			_, err = s.Watch(ctx, &metainternal.ListOptions{ResourceVersion: g.ResourceVersion})
			require.True(t, apierrors.IsResourceExpired(err), "%v", err)
		})
	}
}
