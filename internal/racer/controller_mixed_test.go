// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	authv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"

	"github.com/Azure/unbounded/internal/racer/wire"
)

func TestMixedControllerBootstrapBindings(t *testing.T) {
	a, status, token := authFixture(t)
	ds := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Namespace: "racer", Name: PodNetworkDaemonSetName, UID: "podnet"}}
	require.NoError(t, a.Topology.Create(t.Context(), ds))

	pod := &corev1.Pod{}
	require.NoError(t, a.Topology.Get(t.Context(), client.ObjectKey{Namespace: "racer", Name: "worker-pod"}, pod))
	pod.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(ds, appsv1.SchemeGroupVersion.WithKind("DaemonSet"))}
	require.NoError(t, a.Topology.Update(t.Context(), pod))

	for _, scenario := range []string{"success", "sa", "pod", "node"} {
		bound := *status.DeepCopy()

		switch scenario {
		case "sa":
			bound.User.UID = "old-sa"
		case "pod":
			bound.User.Extra["authentication.kubernetes.io/pod-uid"] = authv1.ExtraValue{"old-pod"}
		case "node":
			bound.User.Extra["authentication.kubernetes.io/node-uid"] = authv1.ExtraValue{"old-node"}
		}

		installReview(t, a, bound, token)

		req := httptest.NewRequest("POST", "https://racer/bootstrap", nil)
		req.Header.Set("Authorization", "Bearer "+token)

		identity, err := a.authority.Authenticate(t.Context(), req)
		if scenario == "success" {
			require.NoError(t, err)
			require.Equal(t, wire.NodeID(testNodeUID), identity.Node())
		} else {
			require.ErrorIs(t, err, wire.Forbidden, scenario)
		}
	}
}

func TestMixedControllerTopologyLiveOwnership(t *testing.T) {
	node := memberNode()
	pod := memberPod("podnet", 1, "192.0.2.2")
	pod.OwnerReferences[0].Name = PodNetworkDaemonSetName
	ds := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Namespace: "racer", Name: PodNetworkDaemonSetName, UID: testDaemonSetUID}}
	r := initializedTopology(t, &node, &pod, ds)
	reconcileTopology(t, r, t.Context())
	require.Len(t, acceptedMembers(t, r), 1)
	before := acceptedMembers(t, r)[testNodeUID]
	r.APIReader = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*appsv1.DaemonSet); ok {
				return errors.New("injected workload read failure")
			}

			return c.Get(ctx, key, obj, opts...)
		},
	})
	_, err := r.Reconcile(t.Context(), ctrl.Request{})
	require.Error(t, err)
	require.Equal(t, before, acceptedMembers(t, r)[testNodeUID])
	r.APIReader = r.Client
	require.NoError(t, r.Delete(t.Context(), ds))
	ds.UID, ds.ResourceVersion = "replacement", ""
	require.NoError(t, r.Create(t.Context(), ds))
	r = Assemble(r.Config, r.Client, r.APIReader).Topology
	reconcileTopology(t, r, t.Context())
	require.Equal(t, before, acceptedMembers(t, r)[testNodeUID])
}

type mixedFailReader struct{ client.Reader }

func (r mixedFailReader) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return errors.New("injected read failure")
}

func TestMixedControllerAuthorization(t *testing.T) {
	cfg := Config{Namespace: "racer", DaemonSetName: DataplaneDaemonSetName, DataplaneServiceAccount: "racer"}
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))

	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: cfg.Namespace, Name: "racer", UID: "sa-current"}}
	ds := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Namespace: cfg.Namespace, Name: PodNetworkDaemonSetName, UID: "pod-current"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sa, ds).Build()
	pod := memberPod("pod", 1, "192.0.2.1")
	pod.Spec.ServiceAccountName = "racer"
	pod.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(ds, appsv1.SchemeGroupVersion.WithKind("DaemonSet"))}
	require.NoError(t, authorizePod(t.Context(), c, cfg, &pod, string(sa.UID)))
	require.ErrorIs(t, authorizePod(t.Context(), c, cfg, &pod, "old-sa"), wire.Forbidden)
	require.ErrorIs(t, authorizePod(t.Context(), mixedFailReader{c}, cfg, &pod, string(sa.UID)), wire.Unavailable)
	ids, err := readManagedWorkloadIdentities(t.Context(), mixedFailReader{c}, cfg)
	require.Error(t, err)
	require.False(t, ids.Owns(&pod))

	for _, mutate := range []func(*corev1.Pod){
		func(p *corev1.Pod) { p.OwnerReferences[0].Name = "arbitrary" },
		func(p *corev1.Pod) { p.OwnerReferences[0].UID = "stale" },
		func(p *corev1.Pod) {
			p.OwnerReferences = nil
			p.Labels = map[string]string{"app": DataplaneDaemonSetName}
		},
		func(p *corev1.Pod) { p.Status.Phase = corev1.PodFailed },
		func(p *corev1.Pod) { p.Spec.ServiceAccountName = "other" },
		func(p *corev1.Pod) { p.UID = "" },
		func(p *corev1.Pod) { p.DeletionTimestamp = &metav1.Time{} },
	} {
		bad := pod.DeepCopy()
		mutate(bad)
		require.ErrorIs(t, authorizePod(t.Context(), c, cfg, bad, string(sa.UID)), wire.Forbidden)
	}

	require.NoError(t, c.Delete(t.Context(), ds))
	require.ErrorIs(t, authorizePod(t.Context(), c, cfg, &pod, string(sa.UID)), wire.Forbidden)
	ds.ResourceVersion, ds.UID = "", "recreated"
	require.NoError(t, c.Create(t.Context(), ds))
	require.ErrorIs(t, authorizePod(t.Context(), c, cfg, &pod, string(sa.UID)), wire.Forbidden)
	pod.OwnerReferences[0].UID = ds.UID
	require.NoError(t, authorizePod(t.Context(), c, cfg, &pod, string(sa.UID)))

	ds.Finalizers = []string{"test/hold"}
	require.NoError(t, c.Update(t.Context(), ds))
	require.NoError(t, c.Delete(t.Context(), ds))
	require.ErrorIs(t, authorizePod(t.Context(), c, cfg, &pod, string(sa.UID)), wire.Forbidden)
}

func TestMixedControllerEvents(t *testing.T) {
	cfg := Config{Namespace: "racer", DaemonSetName: DataplaneDaemonSetName}
	p := memberPod("pod", 1, "192.0.2.1")
	p.OwnerReferences[0].Name = PodNetworkDaemonSetName
	pred := managedPodChanges(cfg)
	require.True(t, pred.Create(event.CreateEvent{Object: &p}))
	require.True(t, pred.Delete(event.DeleteEvent{Object: &p}))
	changed := p.DeepCopy()
	changed.Status.PodIP = "192.0.2.2"
	require.True(t, pred.Update(event.UpdateEvent{ObjectOld: &p, ObjectNew: changed}))
	changed = p.DeepCopy()
	changed.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	require.False(t, pred.Update(event.UpdateEvent{ObjectOld: &p, ObjectNew: changed}))
	p.OwnerReferences[0].Name = "arbitrary"
	require.False(t, pred.Create(event.CreateEvent{Object: &p}))

	ds := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Namespace: "racer", Name: PodNetworkDaemonSetName}}
	require.True(t, namedChanges(cfg.Namespace, managedWorkloadNames(cfg)...).Create(event.CreateEvent{Object: ds}))
}

func TestMixedControllerMembership(t *testing.T) {
	node := memberNode()
	node.Annotations = map[string]string{enrolledSharesAnnotation: "8"}
	host := memberPod("host", 1, "192.0.2.1")
	host.OwnerReferences[0].Name = DataplaneDaemonSetName
	pod := memberPod("pod", 2, "192.0.2.2")
	pod.OwnerReferences[0].Name, pod.OwnerReferences[0].UID = PodNetworkDaemonSetName, "podnet"
	hostDS := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Namespace: "racer", Name: DataplaneDaemonSetName, UID: testDaemonSetUID}}
	podDS := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Namespace: "racer", Name: PodNetworkDaemonSetName, UID: "podnet"}}
	r := initializedTopology(t, &node, &host, hostDS, podDS)
	r.Config.PeerPort = 7443
	reconcileTopology(t, r, t.Context())
	require.NoError(t, r.Create(t.Context(), &pod))
	reconcileTopology(t, r, t.Context())
	after := acceptedMembers(t, r)
	require.Len(t, after, 1)
	require.Equal(t, uint32(8), after[testNodeUID].Shares)
	require.Equal(t, "192.0.2.2:7443", after[testNodeUID].PeerEndpoint)

	require.NoError(t, r.Delete(t.Context(), &host))
	require.NoError(t, r.Delete(t.Context(), podDS))
	podDS.UID, podDS.ResourceVersion = "recreated", ""
	require.NoError(t, r.Create(t.Context(), podDS))
	r = Assemble(r.Config, r.Client, r.APIReader).Topology
	reconcileTopology(t, r, t.Context())
	require.Equal(t, after, acceptedMembers(t, r), "restart retains UID-bound last admitted endpoint")
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(&node), &node))
	delete(node.Annotations, admittedMemberAnnotation)
	require.NoError(t, r.Update(t.Context(), &node))
	r = Assemble(r.Config, r.Client, r.APIReader).Topology
	reconcileTopology(t, r, t.Context())
	require.Empty(t, acceptedMembers(t, r), "stale owner cannot admit a new member")

	pod.OwnerReferences[0].UID = hostDS.UID
	pod.OwnerReferences[0].Name = "arbitrary"
	require.NoError(t, r.Update(t.Context(), &pod))
	reconcileTopology(t, r, t.Context())
	require.Empty(t, acceptedMembers(t, r), "a live UID with the wrong owner name cannot admit a member")
}
