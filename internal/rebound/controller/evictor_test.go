// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clocktesting "k8s.io/utils/clock/testing"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/Azure/unbounded/internal/rebound/ledger"
)

func pod(namespace, name, node string, created time.Time) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, UID: types.UID(namespace + "/" + name), CreationTimestamp: metav1.Time{Time: created}},
		Spec:       corev1.PodSpec{NodeName: node},
	}
}

func newEvictor(t *testing.T, cm *corev1.ConfigMap, funcs *interceptor.Funcs, pods ...*corev1.Pod) (*evictor, client.Client) {
	t.Helper()

	objects := []client.Object{}
	if cm != nil {
		objects = append(objects, cm)
	}

	for _, p := range pods {
		objects = append(objects, p)
	}

	b := clientBuilder(t, objects...).WithIndex(&corev1.Pod{}, "spec.nodeName", func(obj client.Object) []string {
		return []string{obj.(*corev1.Pod).Spec.NodeName}
	})
	if funcs != nil {
		b = b.WithInterceptorFuncs(*funcs)
	}

	c := b.Build()

	return &evictor{
		client:    c,
		apiReader: c,
		namespace: "system",
		exempt:    map[string]bool{"system": true, "kube-system": true},
		clock:     clocktesting.NewFakeClock(testNow),
	}, c
}

func remaining(t *testing.T, c client.Client) []string {
	t.Helper()

	var pods corev1.PodList
	require.NoError(t, c.List(t.Context(), &pods))

	names := []string{}
	for _, p := range pods.Items {
		names = append(names, p.Namespace+"/"+p.Name)
	}

	return names
}

func TestEvictorActiveGrant(t *testing.T) {
	g := grant("tenant", "g", "a", time.Hour)
	g.UID = "u1"
	g.CreatedAt = testNow.Add(-time.Minute)

	mirror := pod("other", "mirror", "a", testNow)
	mirror.Annotations = map[string]string{corev1.MirrorPodAnnotationKey: "x"}
	mirror.OwnerReferences = []metav1.OwnerReference{{APIVersion: "v1", Kind: "Node", Name: "a", UID: "node-a", Controller: ptr.To(true)}}
	forged := pod("other", "forged", "a", testNow)
	forged.Annotations = map[string]string{corev1.MirrorPodAnnotationKey: "x"}
	wrongNode := forged.DeepCopy()
	wrongNode.Name, wrongNode.UID = "wrong-node", "other/wrong-node"
	wrongNode.OwnerReferences = []metav1.OwnerReference{{APIVersion: "v1", Kind: "Node", Name: "b", UID: "node-b", Controller: ptr.To(true)}}
	done := pod("other", "done", "a", testNow)
	done.Status.Phase = corev1.PodSucceeded
	failed := pod("other", "failed", "a", testNow)
	failed.Status.Phase = corev1.PodFailed

	r, c := newEvictor(t, configMap(t, g), nil,
		pod("other", "intruder", "a", testNow),
		pod("tenant", "own", "a", testNow),
		pod("tenant", "before", "a", testNow.Add(-time.Hour)),
		pod("kube-system", "proxy", "a", testNow),
		pod("system", "agent", "a", testNow),
		pod("other", "elsewhere", "b", testNow),
		mirror, forged, wrongNode, done, failed,
	)

	result, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "a"}})
	require.NoError(t, err)
	require.Equal(t, sweepInterval, result.RequeueAfter)
	require.ElementsMatch(t, []string{
		"tenant/own", "tenant/before", "kube-system/proxy", "system/agent", "other/elsewhere",
		"other/mirror", "other/done", "other/failed",
	}, remaining(t, c))
}

func TestEvictorReadsLiveLedger(t *testing.T) {
	// The node moved from tenant-a's grant to tenant-b's grant. The cache still
	// has the old ledger, but the pod list already has tenant-b's pods.
	old := grant("tenant-a", "g", "n1", time.Hour)
	old.UID = "a"
	current := grant("tenant-b", "g", "n1", time.Hour)
	current.UID = "b"
	stale := configMap(t, old)

	r, c := newEvictor(t, configMap(t, current), nil,
		pod("tenant-a", "p", "n1", testNow),
		pod("tenant-b", "p", "n1", testNow),
	)
	r.client = interceptor.NewClient(c.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if cm, ok := obj.(*corev1.ConfigMap); ok {
				stale.DeepCopyInto(cm)
				return nil
			}

			return c.Get(ctx, key, obj, opts...)
		},
	})

	_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "n1"}})
	require.NoError(t, err)
	require.Equal(t, []string{"tenant-b/p"}, remaining(t, c))
}

func TestEvictorLedgerChangesDuringPodList(t *testing.T) {
	old := grant("tenant-a", "g", "n1", time.Hour)
	old.UID = "a"
	renewed := grant("tenant-a", "g", "n1", 2*time.Hour)
	renewed.UID = "a"
	handoff := grant("tenant-b", "g", "n1", time.Hour)
	handoff.UID = "b"

	// update replaces the ledger with one holding grants, as another writer would.
	update := func(ctx context.Context, c client.WithWatch, grants ...*ledger.Grant) error {
		var cm corev1.ConfigMap
		if err := c.Get(ctx, types.NamespacedName{Namespace: "system", Name: ledger.ConfigMapName}, &cm); err != nil {
			return err
		}

		cm.Data = configMap(t, grants...).Data

		return c.Update(ctx, &cm)
	}

	tests := []struct {
		name string
		// during runs inside each pod list, after the list returns.
		during  func(ctx context.Context, c client.WithWatch, lists int) error
		lists   int
		wantErr bool
		want    []string
	}{
		{
			name: "handoff during first list",
			during: func(ctx context.Context, c client.WithWatch, lists int) error {
				if lists > 1 {
					return nil
				}

				// tenant-a releases the node, tenant-b takes it and creates a pod.
				if err := update(ctx, c, handoff); err != nil {
					return err
				}

				return c.Create(ctx, pod("tenant-b", "new", "n1", testNow))
			},
			lists: 2,
			// The listed tenant-b pod was not judged against tenant-a's grant.
			// The second list sees both pods under tenant-b's grant.
			want: []string{"tenant-b/new", "tenant-b/p"},
		},
		{
			name: "grant renewed during first list",
			during: func(ctx context.Context, c client.WithWatch, lists int) error {
				if lists > 1 {
					return nil
				}

				return update(ctx, c, renewed)
			},
			// A new expiry changes the node's records, so the pods are
			// listed again.
			lists: 2,
			want:  []string{"tenant-a/p"},
		},
		{
			name: "ledger keeps changing",
			during: func(ctx context.Context, c client.WithWatch, lists int) error {
				if lists%2 == 1 {
					return update(ctx, c, handoff)
				}

				return update(ctx, c, old)
			},
			lists:   maxListAttempts,
			wantErr: true,
			want:    []string{"tenant-a/p", "tenant-b/p"},
		},
		{
			name: "ledger deleted during list",
			during: func(ctx context.Context, c client.WithWatch, _ int) error {
				return c.Delete(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "system", Name: ledger.ConfigMapName}})
			},
			lists: 1,
			want:  []string{"tenant-a/p", "tenant-b/p"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lists, reconciled := 0, false
			funcs := &interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if err := c.List(ctx, list, opts...); err != nil {
					return err
				}

				if _, ok := list.(*corev1.PodList); !ok || reconciled {
					return nil
				}

				lists++

				return tt.during(ctx, c, lists)
			}}

			r, c := newEvictor(t, configMap(t, old), funcs,
				pod("tenant-a", "p", "n1", testNow),
				pod("tenant-b", "p", "n1", testNow),
			)

			_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "n1"}})
			reconciled = true

			if tt.wantErr {
				require.ErrorContains(t, err, "ledger changed while listing pods")
			} else {
				require.NoError(t, err)
			}

			require.Equal(t, tt.lists, lists)
			require.ElementsMatch(t, tt.want, remaining(t, c))
		})
	}
}

func TestEvictorLedgerChangesDuringDelete(t *testing.T) {
	old := grant("tenant-a", "g", "n1", time.Minute)
	old.UID = "a"
	renewed := grant("tenant-a", "g", "n1", time.Hour)
	renewed.UID = "a"
	handoff := grant("tenant-b", "g", "n1", time.Hour)
	handoff.UID = "b"

	// update replaces the ledger with one holding grants, as another writer would.
	update := func(ctx context.Context, c client.WithWatch, grants ...*ledger.Grant) error {
		var cm corev1.ConfigMap
		if err := c.Get(ctx, types.NamespacedName{Namespace: "system", Name: ledger.ConfigMapName}, &cm); err != nil {
			return err
		}

		cm.Data = configMap(t, grants...).Data

		return c.Update(ctx, &cm)
	}

	tests := []struct {
		name string
		// during runs inside the first pod delete, after the delete returns.
		during func(ctx context.Context, c client.WithWatch, r *evictor) error
		// want and after are the pods left after the first reconcile
		// and after the follow-up reconcile.
		want, after []string
	}{
		{
			name: "handoff during first delete",
			during: func(ctx context.Context, c client.WithWatch, _ *evictor) error {
				return update(ctx, c, handoff)
			},
			want: []string{"tenant-a/p", "tenant-b/p", "tenant-b/q"},
			// tenant-a's pod is now an intruder on tenant-b's node.
			after: []string{"tenant-b/p", "tenant-b/q"},
		},
		{
			name: "grant renewed during first delete",
			during: func(ctx context.Context, c client.WithWatch, _ *evictor) error {
				return update(ctx, c, renewed)
			},
			// A new expiry changes the node's records and stops the sweep.
			want:  []string{"tenant-a/p", "tenant-b/p", "tenant-b/q"},
			after: []string{"tenant-a/p"},
		},
		{
			name: "grant expires during first delete",
			during: func(_ context.Context, _ client.WithWatch, r *evictor) error {
				r.clock.(*clocktesting.FakeClock).Step(2 * time.Minute)
				return nil
			},
			want: []string{"tenant-a/p", "tenant-b/p", "tenant-b/q"},
			// Without a grant or release, the node has no pods to delete.
			after: []string{"tenant-a/p", "tenant-b/p", "tenant-b/q"},
		},
		{
			name: "ledger deleted during first delete",
			during: func(ctx context.Context, c client.WithWatch, _ *evictor) error {
				return c.Delete(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "system", Name: ledger.ConfigMapName}})
			},
			want:  []string{"tenant-a/p", "tenant-b/p", "tenant-b/q"},
			after: []string{"tenant-a/p", "tenant-b/p", "tenant-b/q"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var r *evictor

			deletes := 0
			funcs := &interceptor.Funcs{Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if err := c.Delete(ctx, obj, opts...); err != nil {
					return err
				}

				deletes++
				if deletes > 1 {
					return nil
				}

				return tt.during(ctx, c, r)
			}}

			// The fake client lists pods sorted by namespace, so the
			// intruder is deleted before the tenant-b pods are reached.
			r, c := newEvictor(t, configMap(t, old), funcs,
				pod("intruder", "p", "n1", testNow),
				pod("tenant-a", "p", "n1", testNow),
				pod("tenant-b", "p", "n1", testNow),
				pod("tenant-b", "q", "n1", testNow),
			)
			req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "n1"}}

			result, err := r.Reconcile(t.Context(), req)
			require.NoError(t, err)
			require.Positive(t, result.RequeueAfter)
			require.Equal(t, 1, deletes)
			require.ElementsMatch(t, tt.want, remaining(t, c))

			// The follow-up sweep judges the pods against the new ledger.
			_, err = r.Reconcile(t.Context(), req)
			require.NoError(t, err)
			require.ElementsMatch(t, tt.after, remaining(t, c))
		})
	}
}

// changeOtherNode updates the ledger for a node other than n1, as traffic for
// other grants would. It adds a grant and, from the second call on, removes
// the previous one, which leaves a release record for its node.
func changeOtherNode(ctx context.Context, c client.WithWatch, n int) error {
	var cm corev1.ConfigMap
	if err := c.Get(ctx, types.NamespacedName{Namespace: "system", Name: ledger.ConfigMapName}, &cm); err != nil {
		return err
	}

	l := ledger.Parse(cm.Data)

	g := grant("elsewhere", "g"+strconv.Itoa(n), "other"+strconv.Itoa(n), time.Hour)
	g.UID = types.UID("elsewhere-" + strconv.Itoa(n))
	g.CreatedAt = testNow.Add(-time.Minute)

	if err := l.Add(g); err != nil {
		return err
	}

	if n > 1 {
		l.Remove(types.NamespacedName{Namespace: "elsewhere", Name: "g" + strconv.Itoa(n-1)}, testNow)
	}

	cm.Data = l.Encode()

	return c.Update(ctx, &cm)
}

func TestEvictorIgnoresOtherNodeLedgerChanges(t *testing.T) {
	for name, tc := range map[string]struct {
		// onList and onDelete change the ledger for another node inside
		// every pod list and every pod delete.
		onList, onDelete bool
	}{
		"during pod list": {onList: true},
		"during deletes":  {onDelete: true},
		"during both":     {onList: true, onDelete: true},
	} {
		t.Run(name, func(t *testing.T) {
			lists, deletes, changes := 0, 0, 0
			funcs := &interceptor.Funcs{
				List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if err := c.List(ctx, list, opts...); err != nil {
						return err
					}

					if _, ok := list.(*corev1.PodList); !ok {
						return nil
					}

					lists++

					if !tc.onList {
						return nil
					}

					changes++

					return changeOtherNode(ctx, c, changes)
				},
				Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
					if err := c.Delete(ctx, obj, opts...); err != nil {
						return err
					}

					deletes++

					if !tc.onDelete {
						return nil
					}

					changes++

					return changeOtherNode(ctx, c, changes)
				},
			}

			g := grant("tenant", "g", "n1", time.Hour)
			g.UID = "u1"
			r, c := newEvictor(t, configMap(t, g), funcs,
				pod("intruder", "p", "n1", testNow),
				pod("intruder", "q", "n1", testNow),
				pod("other", "p", "n1", testNow),
				pod("tenant", "p", "n1", testNow),
			)

			result, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "n1"}})
			require.NoError(t, err)
			require.Equal(t, sweepInterval, result.RequeueAfter)
			require.Equal(t, 1, lists)
			require.Equal(t, 3, deletes)
			require.Positive(t, changes)
			require.Equal(t, []string{"tenant/p"}, remaining(t, c))
		})
	}
}

func TestSameNodeRecords(t *testing.T) {
	g := &ledger.Grant{
		Namespace: "tenant", Name: "g", UID: "u1", NamespaceUID: "ns1",
		Nodes: []string{"a"}, CreatedAt: testNow.Add(-time.Hour), ExpiresAt: testNow.Add(time.Hour),
	}
	release := func(uid types.UID) *ledger.Release {
		return &ledger.Release{
			UID: uid, Namespace: "old", NamespaceUID: "ns0", Nodes: []string{"a"},
			CreatedAt: testNow.Add(-2 * time.Hour), ReleasedAt: testNow.Add(-time.Hour), DropAt: testNow.Add(time.Hour),
		}
	}
	releases := []*ledger.Release{release("r1"), release("r2")}

	require.True(t, sameNodeRecords(nil, nil, nil, nil))
	require.True(t, sameNodeRecords(nil, nil, nil, []*ledger.Release{}))
	require.True(t, sameNodeRecords(g, releases, g, releases))

	// Copies from another ledger version, with fields that do not affect
	// the node and with the releases in another order.
	other := *g
	other.Nodes = []string{"a", "b"}
	other.Holder = "someone"
	other.CreatedAt = testNow
	r1, r2 := *releases[0], *releases[1]
	r1.Nodes = []string{"a", "c"}
	require.True(t, sameNodeRecords(g, releases, &other, []*ledger.Release{&r2, &r1}))
	require.Equal(t, types.UID("r1"), releases[0].UID, "inputs must not be reordered")

	require.False(t, sameNodeRecords(g, releases, nil, releases))
	require.False(t, sameNodeRecords(nil, releases, g, releases))
	require.False(t, sameNodeRecords(g, releases, g, releases[:1]))
	require.False(t, sameNodeRecords(g, nil, g, releases))

	for name, change := range map[string]func(*ledger.Grant){
		"namespace":     func(g *ledger.Grant) { g.Namespace = "other" },
		"name":          func(g *ledger.Grant) { g.Name = "other" },
		"uid":           func(g *ledger.Grant) { g.UID = "other" },
		"namespace uid": func(g *ledger.Grant) { g.NamespaceUID = "other" },
		"expiry":        func(g *ledger.Grant) { g.ExpiresAt = g.ExpiresAt.Add(time.Second) },
	} {
		t.Run("grant "+name, func(t *testing.T) {
			changed := *g
			change(&changed)
			require.False(t, sameNodeRecords(g, releases, &changed, releases))
			require.False(t, sameNodeRecords(&changed, releases, g, releases))
		})
	}

	for name, change := range map[string]func(*ledger.Release){
		"uid":           func(r *ledger.Release) { r.UID = "other" },
		"namespace":     func(r *ledger.Release) { r.Namespace = "other" },
		"namespace uid": func(r *ledger.Release) { r.NamespaceUID = "other" },
		"created":       func(r *ledger.Release) { r.CreatedAt = r.CreatedAt.Add(time.Second) },
		"released":      func(r *ledger.Release) { r.ReleasedAt = r.ReleasedAt.Add(time.Second) },
		"drop":          func(r *ledger.Release) { r.DropAt = r.DropAt.Add(time.Second) },
	} {
		t.Run("release "+name, func(t *testing.T) {
			changed := *releases[1]
			change(&changed)
			require.False(t, sameNodeRecords(g, releases, g, []*ledger.Release{releases[0], &changed}))
			require.False(t, sameNodeRecords(g, []*ledger.Release{releases[0], &changed}, g, releases))
		})
	}
}

func TestEvictorDeleteChecksLedgerMetadata(t *testing.T) {
	boom := errors.New("boom")

	// The two reads around the pod list each read metadata, and only the
	// first fetches the full ledger. Each delete then checks metadata.
	for name, tc := range map[string]struct {
		// failFrom is the first metadata read that fails, or 0 for none.
		failFrom int
		// meta and full are the metadata and full ledger reads.
		meta, full int
		// want is the number of pods left after the reconcile.
		want int
	}{
		"unchanged ledger": {meta: 2 + 5, full: 1},
		// The first failed check stops the sweep.
		"metadata error on delete check": {failFrom: 3, meta: 3, full: 1, want: 5},
		"metadata error on first read":   {failFrom: 1, meta: 1, want: 5},
	} {
		t.Run(name, func(t *testing.T) {
			full, meta := 0, 0
			funcs := &interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				switch o := obj.(type) {
				case *corev1.ConfigMap:
					full++
				case *metav1.PartialObjectMetadata:
					require.Equal(t, "ConfigMap", o.Kind)

					meta++

					if tc.failFrom != 0 && meta >= tc.failFrom {
						return boom
					}
				}

				return c.Get(ctx, key, obj, opts...)
			}}

			var pods []*corev1.Pod
			for i := range 5 {
				pods = append(pods, pod("other", "p"+strconv.Itoa(i), "a", testNow))
			}

			r, c := newEvictor(t, configMap(t, grant("tenant", "g", "a", time.Hour)), funcs, pods...)

			_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "a"}})
			if tc.failFrom != 0 {
				require.ErrorIs(t, err, boom)
			} else {
				require.NoError(t, err)
			}

			require.Equal(t, tc.meta, meta)
			require.Equal(t, tc.full, full)
			require.Len(t, remaining(t, c), tc.want)
		})
	}
}

func TestEvictorReusesCachedLedger(t *testing.T) {
	old := grant("tenant-a", "g", "a", time.Hour)
	old.UID = "a"
	handoff := grant("tenant-b", "g", "a", time.Hour)
	handoff.UID = "b"

	full, meta := 0, 0
	funcs := &interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		switch obj.(type) {
		case *corev1.ConfigMap:
			full++
		case *metav1.PartialObjectMetadata:
			meta++
		}

		return c.Get(ctx, key, obj, opts...)
	}}

	r, c := newEvictor(t, configMap(t, old), funcs, pod("tenant-a", "p", "a", testNow))
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "a"}}

	// The read after the pod list reuses the ledger fetched before it.
	result, err := r.Reconcile(t.Context(), req)
	require.NoError(t, err)
	require.Equal(t, sweepInterval, result.RequeueAfter)
	require.Equal(t, 1, full)
	require.Equal(t, 2, meta)

	// Sweeps of an unchanged ledger read only its metadata.
	for range 3 {
		_, err = r.Reconcile(t.Context(), req)
		require.NoError(t, err)
	}

	require.Equal(t, 1, full)
	require.Equal(t, 2+3*2, meta)
	require.Equal(t, []string{"tenant-a/p"}, remaining(t, c))

	// A changed ledger is fetched in full again.
	var cm corev1.ConfigMap
	require.NoError(t, c.Get(t.Context(), types.NamespacedName{Namespace: "system", Name: ledger.ConfigMapName}, &cm))
	cm.Data = configMap(t, handoff).Data
	require.NoError(t, c.Update(t.Context(), &cm))

	full, meta = 0, 0
	_, err = r.Reconcile(t.Context(), req)
	require.NoError(t, err)
	require.Equal(t, 1, full)
	// Two reads around the pod list and one before deleting tenant-a's pod.
	require.Equal(t, 3, meta)
	require.Empty(t, remaining(t, c))
}

func TestEvictorLedgerChangesBetweenReads(t *testing.T) {
	old := grant("tenant-a", "g", "a", time.Hour)
	old.UID = "a"
	handoff := grant("tenant-b", "g", "a", time.Hour)
	handoff.UID = "b"
	boom := errors.New("boom")

	for name, tc := range map[string]struct {
		// between runs after the first metadata read, before the full read.
		between func(ctx context.Context, c client.WithWatch) error
		fullErr error
		lists   int
		want    []string
	}{
		"ledger updated": {
			between: func(ctx context.Context, c client.WithWatch) error {
				var cm corev1.ConfigMap
				if err := c.Get(ctx, types.NamespacedName{Namespace: "system", Name: ledger.ConfigMapName}, &cm); err != nil {
					return err
				}

				cm.Data = configMap(t, handoff).Data

				return c.Update(ctx, &cm)
			},
			// The records carry the version of the full read, so the read
			// after the pod list matches it and one list is enough.
			lists: 1,
			want:  []string{"tenant-b/p"},
		},
		"ledger deleted": {
			between: func(ctx context.Context, c client.WithWatch) error {
				return c.Delete(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "system", Name: ledger.ConfigMapName}})
			},
			want: []string{"tenant-a/p", "tenant-b/p"},
		},
		"full read error": {
			fullErr: boom,
			want:    []string{"tenant-a/p", "tenant-b/p"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			full, meta, lists := 0, 0, 0
			funcs := &interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					switch obj.(type) {
					case *corev1.ConfigMap:
						full++

						if tc.fullErr != nil {
							return tc.fullErr
						}
					case *metav1.PartialObjectMetadata:
						meta++

						if err := c.Get(ctx, key, obj, opts...); err != nil || meta > 1 || tc.between == nil {
							return err
						}

						return tc.between(ctx, c)
					}

					return c.Get(ctx, key, obj, opts...)
				},
				List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if _, ok := list.(*corev1.PodList); ok {
						lists++
					}

					return c.List(ctx, list, opts...)
				},
			}

			r, c := newEvictor(t, configMap(t, old), funcs,
				pod("tenant-a", "p", "a", testNow),
				pod("tenant-b", "p", "a", testNow),
			)

			_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "a"}})
			if tc.fullErr != nil {
				require.ErrorIs(t, err, tc.fullErr)
			} else {
				require.NoError(t, err)
			}

			require.Equal(t, 1, full)
			require.Equal(t, tc.lists, lists)
			require.ElementsMatch(t, tc.want, remaining(t, c))
		})
	}
}

func TestEvictorRelease(t *testing.T) {
	g := grant("tenant", "g", "a", time.Hour)
	g.UID = "u1"
	g.CreatedAt = testNow.Add(-10*time.Minute + 300*time.Millisecond)

	l := ledger.New()
	require.NoError(t, l.Add(g))
	require.True(t, l.Remove(g.Key(), testNow.Add(-time.Minute)))

	cm := configMap(t)
	cm.Data = l.Encode()

	r, c := newEvictor(t, cm, nil,
		// Same second as the grant: creationTimestamp is truncated.
		pod("tenant", "first", "a", testNow.Add(-10*time.Minute)),
		pod("tenant", "during", "a", testNow.Add(-5*time.Minute)),
		pod("tenant", "before", "a", testNow.Add(-11*time.Minute)),
		pod("tenant", "after", "a", testNow.Add(-time.Minute)),
		pod("other", "unrelated", "a", testNow.Add(-5*time.Minute)),
		pod("tenant", "elsewhere", "b", testNow.Add(-5*time.Minute)),
	)

	result, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "a"}})
	require.NoError(t, err)
	require.Equal(t, sweepInterval, result.RequeueAfter)
	require.ElementsMatch(t, []string{"tenant/before", "tenant/after", "other/unrelated", "tenant/elsewhere"}, remaining(t, c))
}

func TestEvictorReleaseNamespaceUID(t *testing.T) {
	boom := errors.New("boom")

	for name, tc := range map[string]struct {
		releaseUID types.UID
		liveUID    types.UID
		getErr     error
		kept       bool
	}{
		"recreated namespace": {releaseUID: "old", liveUID: "new", kept: true},
		"same namespace":      {releaseUID: "old", liveUID: "old"},
		"deleted namespace":   {releaseUID: "old", getErr: apierrors.NewNotFound(corev1.Resource("namespaces"), "tenant"), kept: true},
		"lookup error":        {releaseUID: "old", getErr: boom, kept: true},
		"legacy release":      {liveUID: "new"},
	} {
		t.Run(name, func(t *testing.T) {
			g := grant("tenant", "g", "a", time.Hour)
			g.UID = "u1"
			g.NamespaceUID = tc.releaseUID
			g.CreatedAt = testNow.Add(-10 * time.Minute)

			l := ledger.New()
			require.NoError(t, l.Add(g))
			require.True(t, l.Remove(g.Key(), testNow.Add(-time.Minute)))

			cm := configMap(t)
			cm.Data = l.Encode()

			gets := 0
			funcs := &interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				ns, ok := obj.(*corev1.Namespace)
				if !ok {
					return c.Get(ctx, key, obj, opts...)
				}

				gets++

				if tc.getErr != nil {
					return tc.getErr
				}

				ns.Name, ns.UID = key.Name, tc.liveUID

				return nil
			}}
			r, c := newEvictor(t, cm, funcs,
				pod("tenant", "first", "a", testNow.Add(-5*time.Minute)),
				pod("tenant", "second", "a", testNow.Add(-4*time.Minute)),
			)

			_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "a"}})
			if tc.getErr != nil && !apierrors.IsNotFound(tc.getErr) {
				require.ErrorIs(t, err, tc.getErr)
			} else {
				require.NoError(t, err)
			}

			if tc.kept {
				require.ElementsMatch(t, []string{"tenant/first", "tenant/second"}, remaining(t, c))
			} else {
				require.Empty(t, remaining(t, c))
			}

			switch {
			case tc.releaseUID == "":
				require.Zero(t, gets)
			case tc.getErr != nil && !apierrors.IsNotFound(tc.getErr):
				require.Equal(t, 2, gets)
			default:
				require.Equal(t, 1, gets)
			}
		})
	}
}

func TestEvictorReleaseSecondPrecision(t *testing.T) {
	releasedAt := testNow.Add(-time.Minute + 300*time.Millisecond)
	g := grant("tenant", "old", "a", time.Hour)
	g.UID = "old"
	g.CreatedAt = releasedAt.Add(-time.Minute)

	l := ledger.New()
	require.NoError(t, l.Add(g))
	require.True(t, l.Remove(g.Key(), releasedAt))

	regrant := grant("tenant", "new", "a", time.Hour)
	regrant.UID = "new"
	regrant.CreatedAt = releasedAt
	require.NoError(t, l.Add(regrant))

	cm := configMap(t)
	cm.Data = l.Encode()
	r, c := newEvictor(t, cm, nil,
		pod("tenant", "prior-second", "a", releasedAt.Truncate(time.Second).Add(-time.Second)),
		pod("tenant", "before-release-same-second", "a", releasedAt.Add(-100*time.Millisecond).Truncate(time.Second)),
		pod("tenant", "after-release-same-second", "a", releasedAt.Add(100*time.Millisecond).Truncate(time.Second)),
	)

	_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "a"}})
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"tenant/before-release-same-second", "tenant/after-release-same-second"}, remaining(t, c))
}

func scheduled(p *corev1.Pod, status corev1.ConditionStatus, at time.Time) *corev1.Pod {
	p.Status.Conditions = append(p.Status.Conditions, corev1.PodCondition{
		Type:               corev1.PodScheduled,
		Status:             status,
		LastTransitionTime: metav1.Time{Time: at},
	})

	return p
}

func TestEvictorReleaseScheduledTime(t *testing.T) {
	createdAt := testNow.Add(-10*time.Minute + 300*time.Millisecond)
	releasedAt := testNow.Add(-time.Minute + 300*time.Millisecond)
	before := createdAt.Add(-5 * time.Minute)
	during := testNow.Add(-5 * time.Minute)
	after := testNow.Add(-30 * time.Second)

	for name, tc := range map[string]struct {
		pod  *corev1.Pod
		kept bool
	}{
		// A pod left Pending before the grant and bound during it.
		"created before, scheduled during": {
			pod: scheduled(pod("tenant", "p", "a", before), corev1.ConditionTrue, during),
		},
		"created before, scheduled in grant start second": {
			pod: scheduled(pod("tenant", "p", "a", before), corev1.ConditionTrue, createdAt.Truncate(time.Second)),
		},
		"created and scheduled before": {
			pod:  scheduled(pod("tenant", "p", "a", before), corev1.ConditionTrue, before.Add(time.Second)),
			kept: true,
		},
		"created before, scheduled in prior second": {
			pod:  scheduled(pod("tenant", "p", "a", before), corev1.ConditionTrue, createdAt.Truncate(time.Second).Add(-time.Second)),
			kept: true,
		},
		"created before, not scheduled": {
			pod:  scheduled(pod("tenant", "p", "a", before), corev1.ConditionFalse, during),
			kept: true,
		},
		"created during, not scheduled": {
			pod: scheduled(pod("tenant", "p", "a", during), corev1.ConditionFalse, before),
		},
		"created before, no condition": {
			pod:  pod("tenant", "p", "a", before),
			kept: true,
		},
		"created during, no condition": {
			pod: pod("tenant", "p", "a", during),
		},
		"created before, zero scheduled time": {
			pod:  scheduled(pod("tenant", "p", "a", before), corev1.ConditionTrue, time.Time{}),
			kept: true,
		},
		// The upper bound uses creationTimestamp.
		"created during, scheduled after release": {
			pod: scheduled(pod("tenant", "p", "a", during), corev1.ConditionTrue, after),
		},
		"created after release": {
			pod:  scheduled(pod("tenant", "p", "a", after), corev1.ConditionTrue, after),
			kept: true,
		},
		"other namespace, created before, scheduled during": {
			pod:  scheduled(pod("other", "p", "a", before), corev1.ConditionTrue, during),
			kept: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			g := grant("tenant", "g", "a", time.Hour)
			g.UID = "u1"
			g.CreatedAt = createdAt

			l := ledger.New()
			require.NoError(t, l.Add(g))
			require.True(t, l.Remove(g.Key(), releasedAt))

			cm := configMap(t)
			cm.Data = l.Encode()

			r, c := newEvictor(t, cm, nil, tc.pod)

			_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "a"}})
			require.NoError(t, err)

			if tc.kept {
				require.ElementsMatch(t, []string{tc.pod.Namespace + "/p"}, remaining(t, c))
			} else {
				require.Empty(t, remaining(t, c))
			}
		})
	}
}

func TestPlacedAt(t *testing.T) {
	created := testNow.Add(-time.Hour)

	for name, tc := range map[string]struct {
		conditions []corev1.PodCondition
		want       time.Time
	}{
		"no conditions": {want: created},
		"scheduled later": {
			conditions: []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionTrue, LastTransitionTime: metav1.Time{Time: testNow}}},
			want:       testNow,
		},
		"scheduled earlier": {
			conditions: []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionTrue, LastTransitionTime: metav1.Time{Time: created.Add(-time.Minute)}}},
			want:       created,
		},
		"scheduled zero": {
			conditions: []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionTrue}},
			want:       created,
		},
		"not scheduled": {
			conditions: []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, LastTransitionTime: metav1.Time{Time: testNow}}},
			want:       created,
		},
		"unknown scheduled": {
			conditions: []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionUnknown, LastTransitionTime: metav1.Time{Time: testNow}}},
			want:       created,
		},
		"other condition": {
			conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue, LastTransitionTime: metav1.Time{Time: testNow}}},
			want:       created,
		},
	} {
		t.Run(name, func(t *testing.T) {
			p := pod("tenant", "p", "a", created)
			p.Status.Conditions = tc.conditions
			require.Equal(t, tc.want, placedAt(p))
		})
	}
}

func TestEvictorReleaseLaterSweep(t *testing.T) {
	g := grant("tenant", "g", "a", time.Hour)
	g.UID = "u1"
	g.CreatedAt = testNow.Add(-time.Minute)
	l := ledger.New()
	require.NoError(t, l.Add(g))
	require.True(t, l.Remove(g.Key(), testNow))
	cm := configMap(t)
	cm.Data = l.Encode()
	r, c := newEvictor(t, cm, nil)
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "a"}}

	result, err := r.Reconcile(t.Context(), req)
	require.NoError(t, err)
	require.Equal(t, sweepInterval, result.RequeueAfter)

	require.NoError(t, c.Create(t.Context(), pod("tenant", "late", "a", testNow.Add(-time.Second))))
	r.clock.(*clocktesting.FakeClock).Step(result.RequeueAfter)
	result, err = r.Reconcile(t.Context(), req)
	require.NoError(t, err)
	require.Equal(t, sweepInterval, result.RequeueAfter)
	require.Empty(t, remaining(t, c))

	r.clock.(*clocktesting.FakeClock).SetTime(testNow.Add(ledger.ReleaseRetention))
	result, err = r.Reconcile(t.Context(), req)
	require.NoError(t, err)
	require.Zero(t, result.RequeueAfter)
}

func TestEvictorReleaseRequeueDeadline(t *testing.T) {
	for name, tc := range map[string]struct {
		drops  []time.Time
		active bool
		want   time.Duration
	}{
		"retained":         {drops: []time.Time{testNow.Add(time.Minute)}, want: sweepInterval},
		"earliest drop":    {drops: []time.Time{testNow.Add(time.Minute), testNow.Add(3 * time.Second)}, want: 3 * time.Second},
		"minimum delay":    {drops: []time.Time{testNow.Add(time.Nanosecond)}, want: time.Millisecond},
		"at drop":          {drops: []time.Time{testNow}},
		"past drop":        {drops: []time.Time{testNow.Add(-time.Second)}},
		"mixed drops":      {drops: []time.Time{testNow.Add(-time.Second), testNow.Add(time.Second)}, want: time.Second},
		"legacy record":    {drops: []time.Time{{}}, want: sweepInterval},
		"active at drop":   {drops: []time.Time{testNow}, active: true, want: sweepInterval},
		"active near drop": {drops: []time.Time{testNow.Add(time.Second)}, active: true, want: time.Second},
	} {
		t.Run(name, func(t *testing.T) {
			l := ledger.New()

			for i, drop := range tc.drops {
				g := grant("tenant", "g", "a", time.Hour)
				g.UID = types.UID(strconv.Itoa(i))
				require.NoError(t, l.Add(g))
				require.True(t, l.Remove(g.Key(), testNow.Add(-time.Minute)))

				for _, rel := range l.Releases() {
					if rel.UID == g.UID {
						rel.DropAt = drop
					}
				}
			}

			if tc.active {
				require.NoError(t, l.Add(grant("tenant", "active", "a", time.Hour)))
			}

			cm := configMap(t)
			cm.Data = l.Encode()
			r, _ := newEvictor(t, cm, nil)
			result, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "a"}})
			require.NoError(t, err)
			require.Equal(t, tc.want, result.RequeueAfter)
		})
	}
}

func TestEvictorReleaseIndex(t *testing.T) {
	l := ledger.New()
	for _, g := range []*ledger.Grant{
		{Namespace: "first", Name: "g", UID: "u1", Nodes: []string{"a", "b"}, CreatedAt: testNow.Add(-time.Hour), ExpiresAt: testNow.Add(time.Hour)},
		{Namespace: "second", Name: "g", UID: "u2", Nodes: []string{"a", "c"}, CreatedAt: testNow.Add(-time.Hour), ExpiresAt: testNow.Add(time.Hour)},
		{Namespace: "third", Name: "g", UID: "u3", Nodes: []string{"d"}, CreatedAt: testNow.Add(-time.Hour), ExpiresAt: testNow.Add(time.Hour)},
	} {
		require.NoError(t, l.Add(g))
		require.True(t, l.Remove(g.Key(), testNow))
	}

	cm := configMap(t)
	cm.Data = l.Encode()
	r, c := newEvictor(t, cm, nil,
		pod("first", "p", "a", testNow.Add(-time.Minute)),
		pod("second", "p", "a", testNow.Add(-time.Minute)),
		pod("third", "p", "a", testNow.Add(-time.Minute)),
		pod("first", "on-b", "b", testNow.Add(-time.Minute)),
		pod("second", "on-c", "c", testNow.Add(-time.Minute)),
	)

	_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "a"}})
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"third/p", "first/on-b", "second/on-c"}, remaining(t, c))

	cached := r.cachedLedger
	for _, node := range []string{"b", "c", "missing"} {
		_, err = r.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{Name: node}})
		require.NoError(t, err)
		require.Same(t, cached, r.cachedLedger)
	}

	require.Equal(t, []string{"third/p"}, remaining(t, c))

	l.Reap(testNow.Add(ledger.ReleaseRetention), nil)
	g := &ledger.Grant{Namespace: "third", Name: "new", UID: "u4", Nodes: []string{"a"}, CreatedAt: testNow.Add(-time.Hour), ExpiresAt: testNow.Add(time.Hour)}
	require.NoError(t, l.Add(g))
	require.True(t, l.Remove(g.Key(), testNow))
	require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(cm), cm))
	oldVersion := cm.ResourceVersion
	cm.Data = l.Encode()
	require.NoError(t, c.Update(t.Context(), cm))
	require.NotEqual(t, oldVersion, cm.ResourceVersion)
	require.NoError(t, c.Create(t.Context(), pod("first", "new", "a", testNow.Add(-time.Minute))))

	_, err = r.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "a"}})
	require.NoError(t, err)
	require.NotSame(t, cached, r.cachedLedger)
	require.Equal(t, []string{"first/new"}, remaining(t, c))
	require.NotContains(t, r.releasesByNode, "b")
	require.NotContains(t, r.releasesByNode, "c")
	require.NotContains(t, r.releasesByNode, "d")
}

func TestEvictorNodeRecordsConcurrent(t *testing.T) {
	cm := configMap(t, grant("tenant", "g", "a", time.Hour))
	r := &evictor{}

	for range 10 {
		t.Run("lookup", func(t *testing.T) {
			t.Parallel()

			g, releases := r.nodeRecords(cm, "a")
			require.NotNil(t, g)
			require.Equal(t, "tenant", g.Namespace)
			require.Empty(t, releases)
		})
	}
}

func TestEvictorLateExpiry(t *testing.T) {
	g := grant("tenant", "g", "a", -time.Hour)
	g.UID = "u1"
	g.CreatedAt = g.ExpiresAt.Add(-time.Hour)

	l := ledger.New()
	require.NoError(t, l.Add(g))
	removed, next := l.Reap(testNow, nil)
	require.Equal(t, []*ledger.Grant{g}, removed)
	require.Equal(t, testNow.Add(ledger.ReleaseRetention), next)

	cm := configMap(t)
	cm.Data = l.Encode()
	r, c := newEvictor(t, cm, nil,
		pod("tenant", "during", "a", g.CreatedAt.Add(time.Minute)),
		pod("tenant", "at-expiry", "a", g.ExpiresAt),
		pod("tenant", "after-expiry", "a", g.ExpiresAt.Add(time.Minute)),
	)

	result, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "a"}})
	require.NoError(t, err)
	require.Equal(t, sweepInterval, result.RequeueAfter)
	require.ElementsMatch(t, []string{"tenant/at-expiry", "tenant/after-expiry"}, remaining(t, c))
}

func TestEvictorExpiredGrantIsNotActive(t *testing.T) {
	g := grant("tenant", "g", "a", 0)
	r, c := newEvictor(t, configMap(t, g), nil, pod("other", "p", "a", testNow))

	result, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "a"}})
	require.NoError(t, err)
	require.Zero(t, result.RequeueAfter)
	require.Equal(t, []string{"other/p"}, remaining(t, c))
}

func TestEvictorNoOp(t *testing.T) {
	listed := false
	funcs := &interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
		if _, ok := list.(*corev1.PodList); ok {
			listed = true
		}

		return c.List(ctx, list, opts...)
	}}

	for name, cm := range map[string]*corev1.ConfigMap{
		"missing ledger": nil,
		"other node":     configMap(t, grant("tenant", "g", "b", time.Hour)),
	} {
		t.Run(name, func(t *testing.T) {
			listed = false
			r, c := newEvictor(t, cm, funcs, pod("other", "p", "a", testNow))

			result, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "a"}})
			require.NoError(t, err)
			require.Zero(t, result.RequeueAfter)
			require.False(t, listed)
			require.Equal(t, []string{"other/p"}, remaining(t, c))
		})
	}
}

func TestEvictorDeleteErrors(t *testing.T) {
	boom := errors.New("boom")

	for name, tc := range map[string]struct {
		err     error
		wantErr bool
	}{
		"not found": {err: apierrors.NewNotFound(corev1.Resource("pods"), "p")},
		"conflict":  {err: apierrors.NewConflict(corev1.Resource("pods"), "p", errors.New("uid"))},
		"other":     {err: boom, wantErr: true},
	} {
		t.Run(name, func(t *testing.T) {
			var opts []client.DeleteOption

			funcs := &interceptor.Funcs{Delete: func(_ context.Context, _ client.WithWatch, _ client.Object, o ...client.DeleteOption) error {
				opts = o
				return tc.err
			}}
			r, _ := newEvictor(t, configMap(t, grant("tenant", "g", "a", time.Hour)), funcs, pod("other", "p", "a", testNow))

			_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "a"}})
			if tc.wantErr {
				require.ErrorIs(t, err, boom)
			} else {
				require.NoError(t, err)
			}

			do := &client.DeleteOptions{}
			do.ApplyOptions(opts)
			require.Equal(t, int64(0), *do.GracePeriodSeconds)
			require.Equal(t, types.UID("other/p"), *do.Preconditions.UID)
			require.NotNil(t, do.Preconditions.ResourceVersion)
			require.NotEmpty(t, *do.Preconditions.ResourceVersion)
		})
	}
}

// A pod that finishes after the list must not be deleted: the delete carries
// the listed resourceVersion, so the API server rejects it with a conflict.
func TestEvictorDeleteSkipsPodChangedAfterList(t *testing.T) {
	var changed bool

	funcs := &interceptor.Funcs{Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, o ...client.DeleteOption) error {
		if !changed {
			changed = true

			live := &corev1.Pod{}
			require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(obj), live))

			live.Status.Phase = corev1.PodSucceeded
			require.NoError(t, c.Status().Update(ctx, live))
		}

		return c.Delete(ctx, obj, o...)
	}}
	r, c := newEvictor(t, configMap(t, grant("tenant", "g", "a", time.Hour)), funcs, pod("other", "p", "a", testNow))

	_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "a"}})
	require.NoError(t, err)

	live := &corev1.Pod{}
	require.NoError(t, c.Get(t.Context(), types.NamespacedName{Namespace: "other", Name: "p"}, live))
	require.Equal(t, corev1.PodSucceeded, live.Status.Phase)

	_, err = r.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "a"}})
	require.NoError(t, err)
	require.NoError(t, c.Get(t.Context(), types.NamespacedName{Namespace: "other", Name: "p"}, live))
}

func TestEvictorSkipsPodsAlreadyForceDeleted(t *testing.T) {
	p := pod("other", "p", "a", testNow)
	p.DeletionTimestamp = &metav1.Time{Time: testNow}
	p.DeletionGracePeriodSeconds = new(int64)

	r, _ := newEvictor(t, nil, nil)
	evict, err := r.shouldEvict(p, grant("tenant", "g", "a", time.Hour), nil, nil)
	require.NoError(t, err)
	require.False(t, evict)

	p.DeletionGracePeriodSeconds = nil
	evict, err = r.shouldEvict(p, grant("tenant", "g", "a", time.Hour), nil, nil)
	require.NoError(t, err)
	require.True(t, evict)
}

func TestEvictorMapLedger(t *testing.T) {
	g := grant("tenant", "g", "a", time.Hour)
	gone := grant("tenant", "gone", "b", time.Hour)
	gone.UID = "u2"

	l := ledger.New()
	require.NoError(t, l.Add(g))
	require.NoError(t, l.Add(gone))
	require.True(t, l.Remove(gone.Key(), testNow))

	cm := configMap(t)
	cm.Data = l.Encode()

	r := &evictor{namespace: "system"}
	got := r.mapLedger(t.Context(), cm)
	require.ElementsMatch(t, []ctrl.Request{
		{NamespacedName: types.NamespacedName{Name: "a"}},
		{NamespacedName: types.NamespacedName{Name: "b"}},
	}, got)

	other := cm.DeepCopy()
	other.Namespace = "elsewhere"
	require.Empty(t, r.mapLedger(t.Context(), other))
	require.Empty(t, r.mapLedger(t.Context(), &corev1.Node{}))
}

func TestReaperRequeuesForReleaseDrop(t *testing.T) {
	g := grant("tenant", "g", "a", time.Hour)
	g.UID = "u1"

	l := ledger.New()
	require.NoError(t, l.Add(g))
	require.True(t, l.Remove(g.Key(), testNow))

	cm := configMap(t)
	cm.Data = l.Encode()
	kube := kubefake.NewClientset(cm)
	reader := clientBuilder(t).Build()
	r := &reaper{reader: reader, apiReader: reader, store: &ledger.Store{Client: kube, Namespace: "system"}, clock: clocktesting.NewFakeClock(testNow)}

	result, err := r.Reconcile(t.Context(), ctrl.Request{})
	require.NoError(t, err)
	require.Equal(t, ledger.ReleaseRetention+time.Second, result.RequeueAfter)
}
