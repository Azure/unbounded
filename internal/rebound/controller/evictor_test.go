// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
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
		reader:    c,
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
	require.Zero(t, result.RequeueAfter)
	require.ElementsMatch(t, []string{"tenant/before", "tenant/after", "other/unrelated", "tenant/elsewhere"}, remaining(t, c))
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
	require.Zero(t, result.RequeueAfter)
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
	require.False(t, r.shouldEvict(p, grant("tenant", "g", "a", time.Hour), nil))

	p.DeletionGracePeriodSeconds = nil
	require.True(t, r.shouldEvict(p, grant("tenant", "g", "a", time.Hour), nil))
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
