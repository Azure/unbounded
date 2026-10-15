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
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/client-go/util/workqueue"
	clocktesting "k8s.io/utils/clock/testing"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"

	"github.com/Azure/unbounded/internal/rebound/ledger"
)

var testNow = time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)

func TestSetupInvalidOptions(t *testing.T) {
	require.ErrorContains(t, Setup(nil, Options{}), "requires a ledger store")
	require.ErrorContains(t, Setup(nil, Options{Store: &ledger.Store{}}), "requires a ledger store")
	require.ErrorContains(t, Setup(nil, Options{Namespace: "system", Store: &ledger.Store{Client: kubefake.NewClientset(), Namespace: "other"}}), "namespaces must match")
}

func grant(namespace, name, node string, ttl time.Duration) *ledger.Grant {
	return &ledger.Grant{Namespace: namespace, Name: name, Nodes: []string{node}, ExpiresAt: testNow.Add(ttl)}
}

func configMap(t *testing.T, grants ...*ledger.Grant) *corev1.ConfigMap {
	t.Helper()

	l := ledger.New()
	for _, g := range grants {
		require.NoError(t, l.Add(g))
	}

	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: ledger.ConfigMapName, Namespace: "system", ResourceVersion: "7"}, Data: l.Encode()}
}

func clientBuilder(t *testing.T, objects ...client.Object) *fake.ClientBuilder {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))

	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...)
}

func TestReaper(t *testing.T) {
	live := grant("live", "live", "a", time.Minute)
	later := grant("live", "later", "b", time.Hour)
	cm := configMap(t, live, later, grant("live", "expired", "c", 0), grant("missing", "gone", "d", time.Hour), grant("terminating", "gone", "e", time.Hour))
	cm.Data["stray"] = "nobody/nothing"
	cm.Data["a"] = "wrong/grant"
	cm.Data["_grant.bad"] = "malformed"
	kube := kubefake.NewClientset(cm)
	reader := clientBuilder(t,
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "live"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "terminating", DeletionTimestamp: &metav1.Time{Time: testNow}, Finalizers: []string{"test"}}},
	).Build()
	r := &reaper{reader: reader, apiReader: reader, store: &ledger.Store{Client: kube, Namespace: "system"}, clock: clocktesting.NewFakeClock(testNow)}
	result, err := r.Reconcile(t.Context(), ctrl.Request{})
	require.NoError(t, err)
	require.Equal(t, time.Minute+time.Second, result.RequeueAfter)
	after, err := kube.CoreV1().ConfigMaps("system").Get(t.Context(), ledger.ConfigMapName, metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, configMap(t, live, later).Data, after.Data)
	kube.ClearActions()

	result, err = r.Reconcile(t.Context(), ctrl.Request{})
	require.NoError(t, err)
	require.Equal(t, time.Minute+time.Second, result.RequeueAfter)

	for _, action := range kube.Actions() {
		require.Equal(t, "get", action.GetVerb())
	}
}

func TestReaperEmptyAndNormalization(t *testing.T) {
	for _, malformed := range []bool{false, true} {
		t.Run(map[bool]string{false: "empty", true: "normalize"}[malformed], func(t *testing.T) {
			cm := configMap(t)
			if malformed {
				cm.Data["stray"] = "tenant/grant"
				cm.Data["_grant.tenant.bad"] = "{"
			}

			kube := kubefake.NewClientset(cm)
			r := &reaper{reader: clientBuilder(t).Build(), store: &ledger.Store{Client: kube, Namespace: "system"}, clock: clocktesting.NewFakeClock(testNow)}
			result, err := r.Reconcile(t.Context(), ctrl.Request{})
			require.NoError(t, err)
			require.Zero(t, result.RequeueAfter)

			writes := 0

			for _, action := range kube.Actions() {
				if action.GetVerb() == "update" {
					writes++
				}
			}

			require.Equal(t, map[bool]int{false: 0, true: 1}[malformed], writes)
			after, err := kube.CoreV1().ConfigMaps("system").Get(t.Context(), ledger.ConfigMapName, metav1.GetOptions{})
			require.NoError(t, err)
			require.Empty(t, after.Data)
		})
	}
}

func TestReaperNamespaceErrorDoesNotWrite(t *testing.T) {
	sentinel := errors.New("namespace cache failed")
	cm := configMap(t, grant("tenant", "grant", "node", time.Hour))
	kube := kubefake.NewClientset(cm)
	reader := clientBuilder(t).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return sentinel
		},
	}).Build()
	r := &reaper{reader: reader, store: &ledger.Store{Client: kube, Namespace: "system"}, clock: clocktesting.NewFakeClock(testNow)}
	_, err := r.Reconcile(t.Context(), ctrl.Request{})
	require.ErrorIs(t, err, sentinel)

	for _, action := range kube.Actions() {
		require.Equal(t, "get", action.GetVerb())
	}
}

func TestReaperConfirmsMissingNamespace(t *testing.T) {
	sentinel := errors.New("namespace API failed")
	for _, tt := range []struct {
		name        string
		namespace   *corev1.Namespace
		readError   error
		wantRemoved bool
	}{
		{name: "cache lags", namespace: &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "tenant"}}},
		{name: "missing", wantRemoved: true},
		{name: "terminating", namespace: &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "tenant", DeletionTimestamp: &metav1.Time{Time: testNow}, Finalizers: []string{"test"}}}, wantRemoved: true},
		{name: "API error", readError: sentinel},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cm := configMap(t, grant("tenant", "one", "a", time.Hour), grant("tenant", "two", "b", time.Hour))
			kube := kubefake.NewClientset(cm)

			builder := clientBuilder(t)
			if tt.namespace != nil {
				builder.WithObjects(tt.namespace)
			}

			reads := 0
			apiReader := builder.WithInterceptorFuncs(interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					reads++

					if tt.readError != nil {
						return tt.readError
					}

					return c.Get(ctx, key, obj, opts...)
				},
			}).Build()
			r := &reaper{reader: clientBuilder(t).Build(), apiReader: apiReader, store: &ledger.Store{Client: kube, Namespace: "system"}, clock: clocktesting.NewFakeClock(testNow)}
			result, err := r.Reconcile(t.Context(), ctrl.Request{})
			require.ErrorIs(t, err, tt.readError)
			require.Equal(t, 1, reads)

			if !tt.wantRemoved {
				for _, action := range kube.Actions() {
					require.Equal(t, "get", action.GetVerb())
				}
			}

			after, err := kube.CoreV1().ConfigMaps("system").Get(t.Context(), ledger.ConfigMapName, metav1.GetOptions{})
			require.NoError(t, err)

			if tt.wantRemoved {
				require.Empty(t, after.Data)
				require.Zero(t, result.RequeueAfter)
			} else {
				require.Equal(t, cm.Data, after.Data)

				if tt.readError == nil {
					require.Equal(t, time.Hour+time.Second, result.RequeueAfter)
				}
			}
		})
	}
}

func TestReaperNamespaceUID(t *testing.T) {
	for _, tc := range []struct {
		name        string
		grantUID    types.UID
		cacheUID    types.UID
		liveUID     types.UID
		liveError   error
		wantRemoved bool
		wantReads   int
	}{
		{name: "match", grantUID: "original", cacheUID: "original"},
		{name: "recreated", grantUID: "original", cacheUID: "replacement", liveUID: "replacement", wantRemoved: true, wantReads: 1},
		{name: "stale mismatch", grantUID: "original", cacheUID: "stale", liveUID: "original", wantReads: 1},
		{name: "cache missing live matches", grantUID: "original", liveUID: "original", wantReads: 1},
		{name: "cache missing live differs", grantUID: "original", liveUID: "replacement", wantRemoved: true, wantReads: 1},
		{name: "mismatch live missing", grantUID: "original", cacheUID: "replacement", wantRemoved: true, wantReads: 1},
		{name: "mismatch live error", grantUID: "original", cacheUID: "replacement", liveError: errors.New("namespace API failed"), wantReads: 1},
		{name: "legacy", cacheUID: "replacement"},
		{name: "legacy cache missing", liveUID: "replacement", wantReads: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := grant("tenant", "one", "node", time.Hour)
			g.NamespaceUID = tc.grantUID
			cm := configMap(t, g)
			kube := kubefake.NewClientset(cm)

			cacheBuilder, liveBuilder := clientBuilder(t), clientBuilder(t)
			if tc.cacheUID != "" {
				cacheBuilder.WithObjects(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "tenant", UID: tc.cacheUID}})
			}

			if tc.liveUID != "" {
				liveBuilder.WithObjects(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "tenant", UID: tc.liveUID}})
			}

			reads := 0
			apiReader := liveBuilder.WithInterceptorFuncs(interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					reads++

					if tc.liveError != nil {
						return tc.liveError
					}

					return c.Get(ctx, key, obj, opts...)
				},
			}).Build()
			r := &reaper{reader: cacheBuilder.Build(), apiReader: apiReader, store: &ledger.Store{Client: kube, Namespace: "system"}, clock: clocktesting.NewFakeClock(testNow)}
			_, err := r.Reconcile(t.Context(), ctrl.Request{})
			require.ErrorIs(t, err, tc.liveError)
			require.Equal(t, tc.wantReads, reads)
			after, err := kube.CoreV1().ConfigMaps("system").Get(t.Context(), ledger.ConfigMapName, metav1.GetOptions{})
			require.NoError(t, err)

			if tc.wantRemoved {
				require.Empty(t, after.Data)
			} else {
				require.Equal(t, cm.Data, after.Data)
			}
		})
	}
}

func TestReaperStampsReleaseAfterNamespaceLookup(t *testing.T) {
	g := grant("missing", "gone", "node", time.Hour)
	g.UID = "grant-uid"
	kube := kubefake.NewClientset(configMap(t, g))
	clk := clocktesting.NewFakeClock(testNow)
	reader := clientBuilder(t).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			clk.Step(ledger.HandoffCooldown)

			return c.Get(ctx, key, obj, opts...)
		},
	}).Build()
	r := &reaper{reader: reader, apiReader: reader, store: &ledger.Store{Client: kube, Namespace: "system"}, clock: clk}
	_, err := r.Reconcile(t.Context(), ctrl.Request{})
	require.NoError(t, err)

	afterLookup := testNow.Add(2 * ledger.HandoffCooldown)
	require.Equal(t, afterLookup, clk.Now())

	cm, err := kube.CoreV1().ConfigMaps("system").Get(t.Context(), ledger.ConfigMapName, metav1.GetOptions{})
	require.NoError(t, err)

	l := ledger.Parse(cm.Data)
	require.Empty(t, l.Grants())
	releases := l.Releases()
	require.Len(t, releases, 1)
	require.True(t, afterLookup.Equal(releases[0].ReleasedAt), "ReleasedAt %v, want %v", releases[0].ReleasedAt, afterLookup)
	require.True(t, afterLookup.Add(ledger.ReleaseRetention).Equal(releases[0].DropAt), "DropAt %v", releases[0].DropAt)
	// The live removal is confirmed by a write after it commits.
	require.True(t, afterLookup.Equal(releases[0].ConfirmedAt), "ConfirmedAt %v, want %v", releases[0].ConfirmedAt, afterLookup)

	var cooldown *ledger.CooldownError
	require.ErrorAs(t, l.CheckCooldown([]string{"node"}, clk.Now()), &cooldown)
	require.Equal(t, ledger.HandoffCooldown, cooldown.RetryAfter)
}

// TestReaperConfirmsReleases checks that a sweep confirms a release whose
// follow-up confirmation was lost, so the node is not fenced forever.
func TestReaperConfirmsReleases(t *testing.T) {
	g := grant("tenant", "g", "node", time.Hour)
	g.UID = "grant-uid"

	l := ledger.New()
	require.NoError(t, l.Add(g))
	require.True(t, l.Remove(g.Key(), testNow))
	require.True(t, l.Unconfirmed())

	cm := configMap(t)
	cm.Data = l.Encode()
	kube := kubefake.NewClientset(cm)
	clk := clocktesting.NewFakeClock(testNow.Add(time.Minute))
	reader := clientBuilder(t).Build()
	r := &reaper{reader: reader, apiReader: reader, store: &ledger.Store{Client: kube, Namespace: "system"}, clock: clk}

	_, err := r.Reconcile(t.Context(), ctrl.Request{})
	require.NoError(t, err)

	after, err := kube.CoreV1().ConfigMaps("system").Get(t.Context(), ledger.ConfigMapName, metav1.GetOptions{})
	require.NoError(t, err)

	releases := ledger.Parse(after.Data).Releases()
	require.Len(t, releases, 1)
	require.True(t, clk.Now().Equal(releases[0].ConfirmedAt), "ConfirmedAt %v", releases[0].ConfirmedAt)
}

// TestReaperConfirmFailure checks that a failed confirmation after a live
// removal fails the sweep, so it is retried, and leaves the node fenced.
func TestReaperConfirmFailure(t *testing.T) {
	g := grant("missing", "gone", "node", time.Hour)
	g.UID = "grant-uid"
	kube := kubefake.NewClientset(configMap(t, g))

	updates := 0

	kube.PrependReactor("update", "configmaps", func(clienttesting.Action) (bool, runtime.Object, error) {
		updates++
		if updates > 1 {
			return true, nil, apierrors.NewServiceUnavailable("ledger unavailable")
		}

		return false, nil, nil
	})

	reader := clientBuilder(t).Build()
	r := &reaper{reader: reader, apiReader: reader, store: &ledger.Store{Client: kube, Namespace: "system"}, clock: clocktesting.NewFakeClock(testNow)}

	_, err := r.Reconcile(t.Context(), ctrl.Request{})
	require.True(t, apierrors.IsServiceUnavailable(err), "%v", err)
	require.Equal(t, 2, updates)

	after, err := kube.CoreV1().ConfigMaps("system").Get(t.Context(), ledger.ConfigMapName, metav1.GetOptions{})
	require.NoError(t, err)

	l := ledger.Parse(after.Data)
	require.Empty(t, l.Grants())
	require.True(t, l.Unconfirmed())

	var cooldown *ledger.CooldownError
	require.ErrorAs(t, l.CheckCooldown([]string{"node"}, testNow.Add(time.Hour)), &cooldown)
	require.True(t, cooldown.Unconfirmed)
}

func TestReaperMixedNamespaceUIDs(t *testing.T) {
	old := grant("tenant", "old", "old-node", time.Hour)
	old.NamespaceUID = "old"
	current := grant("tenant", "current", "current-node", time.Hour)
	current.NamespaceUID = "current"
	legacy := grant("tenant", "legacy", "legacy-node", time.Hour)
	kube := kubefake.NewClientset(configMap(t, old, current, legacy))
	reader := clientBuilder(t, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "tenant", UID: "current"}}).Build()
	r := &reaper{reader: reader, apiReader: reader, store: &ledger.Store{Client: kube, Namespace: "system"}, clock: clocktesting.NewFakeClock(testNow)}
	_, err := r.Reconcile(t.Context(), ctrl.Request{})
	require.NoError(t, err)
	after, err := kube.CoreV1().ConfigMaps("system").Get(t.Context(), ledger.ConfigMapName, metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, configMap(t, current, legacy).Data, after.Data)
}

func TestMirror(t *testing.T) {
	unrelated := corev1.Taint{Key: "other", Value: "value", Effect: corev1.TaintEffectNoSchedule}

	for _, tt := range []struct {
		name         string
		currentNS    string
		currentGrant string
		grant        *ledger.Grant
		wantNS       string
		wantGrant    string
		wantPatches  int
		wantRequeue  time.Duration
	}{
		{name: "add", grant: grant("tenant", "grant", "node", time.Minute), wantNS: "tenant", wantGrant: "grant", wantPatches: 1, wantRequeue: time.Minute},
		{name: "remove", currentNS: "tenant", currentGrant: "grant", wantPatches: 1},
		{name: "move", currentNS: "old", currentGrant: "old", grant: grant("new", "new", "node", time.Minute), wantNS: "new", wantGrant: "new", wantPatches: 1, wantRequeue: time.Minute},
		{name: "expired", currentNS: "tenant", currentGrant: "grant", grant: grant("tenant", "grant", "node", 0), wantPatches: 1},
		{name: "in sync", currentNS: "tenant", currentGrant: "grant", grant: grant("tenant", "grant", "node", time.Minute), wantNS: "tenant", wantGrant: "grant", wantRequeue: time.Minute},
		{name: "ungranted"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node", Labels: map[string]string{"other": "keep"}}, Spec: corev1.NodeSpec{Taints: []corev1.Taint{unrelated}}}
			if tt.currentNS != "" {
				mirrorGrant(node, grant(tt.currentNS, tt.currentGrant, "node", time.Minute))
			}

			cm := configMap(t)
			if tt.grant != nil {
				cm = configMap(t, tt.grant)
			}

			patches := 0
			c := clientBuilder(t, node, cm).WithInterceptorFuncs(interceptor.Funcs{
				Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
					patches++
					data, err := patch.Data(obj)
					require.NoError(t, err)
					require.Contains(t, string(data), `"resourceVersion":`)

					return c.Patch(ctx, obj, patch, opts...)
				},
			}).Build()
			r := &nodeMirror{client: c, reader: c, namespace: "system", clock: clocktesting.NewFakeClock(testNow)}
			result, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "node"}})
			require.NoError(t, err)
			require.Equal(t, tt.wantRequeue, result.RequeueAfter)
			require.Equal(t, tt.wantPatches, patches)

			var after corev1.Node
			require.NoError(t, c.Get(t.Context(), types.NamespacedName{Name: "node"}, &after))
			require.Equal(t, "keep", after.Labels["other"])
			require.Equal(t, tt.wantNS, after.Labels[LabelNamespace])
			require.Equal(t, tt.wantGrant, after.Labels[LabelGrant])

			wantTaints := []corev1.Taint{unrelated}
			if tt.wantNS != "" {
				wantTaints = append(wantTaints, corev1.Taint{Key: TaintKey, Value: tt.wantNS, Effect: corev1.TaintEffectPreferNoSchedule})
			} else {
				require.NotContains(t, after.Labels, LabelNamespace)
				require.NotContains(t, after.Labels, LabelGrant)
			}

			require.Equal(t, wantTaints, after.Spec.Taints)
		})
	}
}

func TestMirrorExpiryWithoutReaping(t *testing.T) {
	cm := configMap(t, grant("tenant", "grant", "node", time.Minute))
	c := clientBuilder(t, cm, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node"}}).Build()
	clock := clocktesting.NewFakeClock(testNow)
	r := &nodeMirror{client: c, reader: c, namespace: "system", clock: clock}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "node"}}
	result, err := r.Reconcile(t.Context(), req)
	require.NoError(t, err)
	require.Equal(t, time.Minute, result.RequeueAfter)
	clock.Step(time.Minute)

	result, err = r.Reconcile(t.Context(), req)
	require.NoError(t, err)
	require.Zero(t, result.RequeueAfter)

	var node corev1.Node
	require.NoError(t, c.Get(t.Context(), req.NamespacedName, &node))
	require.Empty(t, node.Labels)
	require.Empty(t, node.Spec.Taints)
	require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(cm), cm))
	require.NotEmpty(t, cm.Data)
}

func TestMirrorMissingObjectsAndErrors(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node"}}
	mirrorGrant(node, grant("old", "grant", "node", time.Minute))
	c := clientBuilder(t, node).Build()
	r := &nodeMirror{client: c, reader: c, namespace: "system", clock: clocktesting.NewFakeClock(testNow)}
	_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "missing"}})
	require.NoError(t, err)
	_, err = r.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "node"}})
	require.NoError(t, err)
	require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(node), node))
	require.Empty(t, node.Labels)
	require.Empty(t, node.Spec.Taints)

	sentinel := errors.New("cache failed")
	c = clientBuilder(t, node).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*corev1.ConfigMap); ok {
				return sentinel
			}

			return c.Get(ctx, key, obj, opts...)
		},
	}).Build()
	r.reader = c
	_, err = r.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "node"}})
	require.ErrorIs(t, err, sentinel)
}

func TestMirrorPatchConflict(t *testing.T) {
	c := clientBuilder(t, configMap(t, grant("tenant", "grant", "node", time.Minute)), &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node"}}).
		WithInterceptorFuncs(interceptor.Funcs{Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
			return apierrors.NewConflict(corev1.Resource("nodes"), "node", errors.New("changed"))
		}}).Build()
	r := &nodeMirror{client: c, reader: c, namespace: "system", clock: clocktesting.NewFakeClock(testNow)}
	_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "node"}})
	require.True(t, apierrors.IsConflict(err))
}

func TestMirrorRepairsTaints(t *testing.T) {
	node := &corev1.Node{Spec: corev1.NodeSpec{Taints: []corev1.Taint{
		{Key: TaintKey, Value: "wrong", Effect: corev1.TaintEffectNoSchedule},
		{Key: "other", Effect: corev1.TaintEffectNoExecute},
		{Key: TaintKey, Value: "duplicate", Effect: corev1.TaintEffectPreferNoSchedule},
	}}}
	mirrorGrant(node, grant("tenant", "grant", "node", time.Minute))
	require.Equal(t, []corev1.Taint{
		{Key: TaintKey, Value: "tenant", Effect: corev1.TaintEffectPreferNoSchedule},
		{Key: "other", Effect: corev1.TaintEffectNoExecute},
	}, node.Spec.Taints)
}

func TestMirrorLedgerNodesAndPredicates(t *testing.T) {
	cm := configMap(t, grant("tenant", "one", "desired", time.Minute), grant("tenant", "two", "both", time.Minute))
	r := &nodeMirror{namespace: "system"}
	require.ElementsMatch(t, []ctrl.Request{
		{NamespacedName: types.NamespacedName{Name: "both"}},
		{NamespacedName: types.NamespacedName{Name: "desired"}},
	}, r.ledgerNodes(cm))

	p := ledgerPredicate("system")
	require.True(t, p.Create(event.CreateEvent{Object: cm}))
	require.True(t, p.Delete(event.DeleteEvent{Object: cm}))

	other := cm.DeepCopy()
	other.Namespace = "other"
	require.False(t, p.Create(event.CreateEvent{Object: other}))
	require.Empty(t, r.ledgerNodes(other))
	other.Namespace, other.Name = "system", "other"
	require.False(t, p.Create(event.CreateEvent{Object: other}))
	require.Empty(t, r.ledgerNodes(other))
	require.Empty(t, r.ledgerNodes(&corev1.Node{}))
	require.Equal(t, singleton(t.Context(), cm), singleton(t.Context(), &corev1.Namespace{}))
}

// A grant removed from the ledger appears only in the old object, so the
// update handler must enqueue nodes from both sides.
func TestMirrorLedgerHandlerEnqueuesRemovedNodes(t *testing.T) {
	r := &nodeMirror{namespace: "system"}
	oldCM := configMap(t, grant("tenant", "gone", "removed", time.Minute), grant("tenant", "kept", "kept", time.Minute))
	newCM := configMap(t, grant("tenant", "kept", "kept", time.Minute), grant("tenant", "new", "added", time.Minute))

	drain := func(q workqueue.TypedRateLimitingInterface[ctrl.Request]) []string {
		var names []string

		for q.Len() > 0 {
			req, _ := q.Get()
			names = append(names, req.Name)
			q.Done(req)
		}

		return names
	}
	newQueue := func() workqueue.TypedRateLimitingInterface[ctrl.Request] {
		q := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[ctrl.Request]())
		t.Cleanup(q.ShutDown)

		return q
	}

	h := r.ledgerHandler()

	q := newQueue()
	h.Update(t.Context(), event.UpdateEvent{ObjectOld: oldCM, ObjectNew: newCM}, q)
	require.ElementsMatch(t, []string{"removed", "kept", "added"}, drain(q))

	q = newQueue()
	h.Delete(t.Context(), event.DeleteEvent{Object: oldCM}, q)
	require.ElementsMatch(t, []string{"removed", "kept"}, drain(q))

	q = newQueue()
	h.Create(t.Context(), event.CreateEvent{Object: newCM}, q)
	require.ElementsMatch(t, []string{"kept", "added"}, drain(q))

	q = newQueue()
	h.Generic(t.Context(), event.GenericEvent{Object: newCM}, q)
	require.ElementsMatch(t, []string{"kept", "added"}, drain(q))
}

func TestMirrorParsesLedgerOncePerVersion(t *testing.T) {
	r := &nodeMirror{namespace: "system"}
	cm := configMap(t, grant("tenant", "g", "a", time.Minute))

	g := r.nodeGrant(cm, "a")
	require.NotNil(t, g)
	require.Equal(t, "g", g.Name)

	cached := r.cachedLedger
	require.Nil(t, r.nodeGrant(cm, "b"))
	require.Same(t, cached, r.cachedLedger)

	// Same resourceVersion: the cached parse is reused even if the data differs.
	cm.Data = configMap(t, grant("tenant", "other", "b", time.Minute)).Data
	require.Equal(t, "g", r.nodeGrant(cm, "a").Name)
	require.Same(t, cached, r.cachedLedger)

	// New resourceVersion: the ledger is parsed again.
	cm.ResourceVersion = "8"
	require.Nil(t, r.nodeGrant(cm, "a"))
	require.Equal(t, "other", r.nodeGrant(cm, "b").Name)
	require.NotSame(t, cached, r.cachedLedger)
}
