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
		})
	}
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
