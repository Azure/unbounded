// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"testing"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/Azure/unbounded/internal/racer/wire"
)

func TestMixedNetworkConfiguration(t *testing.T) {
	values := map[string]string{
		"RACER_CLUSTER_ID":  "11111111-1111-1111-1111-111111111111",
		"RACER_CONTROL_URL": "https://controller:8443", "RACER_DATAPLANE_IMAGE": "racer:5e1",
		"RACER_HOST_NETWORK": "true",
	}
	lookup := func(key string) (string, bool) { value, ok := values[key]; return value, ok }

	for _, input := range []string{`[]`, `["node-b","node-a"]`} {
		values["RACER_POD_NETWORK_NODES"] = input
		_, err := WorkloadConfigFromLookup(lookup)
		require.NoError(t, err)
	}

	for _, input := range []string{"", "null", `{}`, `"node-a"`, `[1]`, `[null]`, `[""]`, `["Node-A"]`, `["node-a","node-a"]`, `["node-a"] trailing`} {
		values["RACER_POD_NETWORK_NODES"] = input
		_, err := WorkloadConfigFromLookup(lookup)
		require.ErrorIs(t, err, wire.InvalidRequest, input)
	}

	values["RACER_POD_NETWORK_NODES"] = `["node-a"]`
	values["RACER_HOST_NETWORK"] = "false"
	_, err := WorkloadConfigFromLookup(lookup)
	require.ErrorIs(t, err, wire.InvalidRequest)
}

func TestMixedNetworkBuilders(t *testing.T) {
	cfg := workloadConfig(t)
	legacy, err := DesiredDaemonSet(cfg)
	require.NoError(t, err)
	sets, err := DesiredDaemonSets(cfg)
	require.NoError(t, err)
	require.Equal(t, []*appsv1.DaemonSet{legacy}, sets)

	cfg.HostNetwork = true
	cfg.PeerPort, cfg.DiagnosticsPort = 18082, 19090
	cfg.PodNetworkNodes = []string{"node-b", "node-a"}
	_, err = DesiredDaemonSet(cfg)
	require.ErrorIs(t, err, wire.InvalidRequest, "legacy planner must fail closed")
	sets, err = DesiredDaemonSets(cfg)
	require.NoError(t, err)
	require.Len(t, sets, 2)
	host, pod := sets[0], sets[1]
	require.Equal(t, DataplaneDaemonSetName, host.Name)
	require.Equal(t, PodNetworkDaemonSetName, pod.Name)
	require.Equal(t, legacy.Spec.Selector, host.Spec.Selector)
	require.True(t, host.Spec.Template.Spec.HostNetwork)
	require.False(t, pod.Spec.Template.Spec.HostNetwork)
	require.Equal(t, corev1.DNSClusterFirst, pod.Spec.Template.Spec.DNSPolicy)
	require.Equal(t, host.Spec.Template.Spec.Volumes, pod.Spec.Template.Spec.Volumes)
	require.Equal(t, host.Spec.Template.Spec.ServiceAccountName, pod.Spec.Template.Spec.ServiceAccountName)
	require.Equal(t, host.Spec.Template.Spec.Containers, pod.Spec.Template.Spec.Containers)

	for i, ds := range sets {
		selector, err := metav1.LabelSelectorAsSelector(ds.Spec.Selector)
		require.NoError(t, err)
		require.True(t, selector.Matches(labels.Set(ds.Spec.Template.Labels)))
		require.False(t, selector.Matches(labels.Set(sets[1-i].Spec.Template.Labels)))
	}

	hostTerms := host.Spec.Template.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	podTerms := pod.Spec.Template.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms

	require.Len(t, hostTerms, 1)
	require.Len(t, hostTerms[0].MatchFields, 2)
	require.Len(t, podTerms, 2)

	for i, node := range []string{"node-a", "node-b"} {
		require.Equal(t, corev1.NodeSelectorRequirement{Key: "metadata.name", Operator: corev1.NodeSelectorOpNotIn, Values: []string{node}}, hostTerms[0].MatchFields[i])
		require.Equal(t, []corev1.NodeSelectorRequirement{{Key: "metadata.name", Operator: corev1.NodeSelectorOpIn, Values: []string{node}}}, podTerms[i].MatchFields)
		require.Equal(t, hostTerms[0].MatchExpressions, podTerms[i].MatchExpressions)
	}

	require.Equal(t, []string{"node-b", "node-a"}, cfg.PodNetworkNodes)
}

func TestMixedNetworkIdentities(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))

	host := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Namespace: "racer", Name: DataplaneDaemonSetName, UID: "host-current"}}
	podnet := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Namespace: "racer", Name: PodNetworkDaemonSetName, UID: "pod-current"}}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(host, podnet).Build()
	ids, err := ReadDataplaneWorkloadIdentities(t.Context(), reader, "racer")
	require.NoError(t, err)

	for _, ds := range []*appsv1.DaemonSet{host, podnet} {
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "racer", OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(ds, appsv1.SchemeGroupVersion.WithKind("DaemonSet"))}}}
		require.True(t, ids.Owns(pod))

		for _, mutate := range []func(*corev1.Pod){
			func(p *corev1.Pod) { p.Namespace = "other" },
			func(p *corev1.Pod) { p.OwnerReferences[0].UID = "stale" },
			func(p *corev1.Pod) { p.OwnerReferences[0].UID = "" },
			func(p *corev1.Pod) { p.OwnerReferences[0].Name = "arbitrary" },
			func(p *corev1.Pod) { p.OwnerReferences[0].Controller = ptr.To(false) },
			func(p *corev1.Pod) { p.OwnerReferences[0].APIVersion = "apps/v2" },
			func(p *corev1.Pod) { p.OwnerReferences = nil; p.Labels = ds.Labels },
		} {
			bad := pod.DeepCopy()
			mutate(bad)
			require.False(t, ids.Owns(bad))
		}

		require.False(t, (DataplaneWorkloadIdentities{}).Owns(pod))
	}

	require.False(t, ids.Owns(nil))
	require.NoError(t, reader.Delete(t.Context(), podnet))
	ids, err = ReadDataplaneWorkloadIdentities(t.Context(), reader, "racer")
	require.NoError(t, err)
	require.Empty(t, ids.uids[1])
	require.Equal(t, host.UID, ids.uids[0])
}
