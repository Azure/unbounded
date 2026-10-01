// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"

	machinav1 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/racer/wire"
)

func TestMemberSiteLabelsOverrideHistory(t *testing.T) {
	for _, tc := range []struct {
		name   string
		labels map[string]string
		want   string
	}{
		{"unlabeled", nil, ""},
		{"canonical", map[string]string{machinav1.MachineSiteLabelKey: "site-a"}, "site-a"},
		{"deprecated ignored", map[string]string{"net.unbounded-cloud.io/site": "site-b"}, ""},
		{"canonical only", map[string]string{machinav1.MachineSiteLabelKey: "site-a", "net.unbounded-cloud.io/site": "site-b"}, "site-a"},
		{"empty canonical no fallback", map[string]string{machinav1.MachineSiteLabelKey: "", "net.unbounded-cloud.io/site": "site-b"}, ""},
		{"empty labels", map[string]string{machinav1.MachineSiteLabelKey: ""}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, mode := range []string{"cold", "memory", "restart"} {
				t.Run(mode, func(t *testing.T) {
					node := memberNode()
					node.Labels = tc.labels
					pods := map[string][]corev1.Pod{node.Name: {memberPod("a", 1, "192.0.2.1")}}

					var accepted AcceptedMembers

					previous := wire.Member{Node: testNodeUID, Shares: 8, PeerEndpoint: "192.0.2.1:7443", Rails: []wire.Rail{{Rail: 1, Fabric: "fabric-a"}}, AlignmentEnabled: true, Site: "stale-site"}

					if mode != "cold" {
						node.Annotations = map[string]string{wire.RailsAnnotation: "malformed"}
						pods = nil

						if mode == "memory" {
							accepted = AcceptedMembers{testNodeUID: previous}
						} else {
							encoded, err := json.Marshal(previous)
							require.NoError(t, err)

							node.Annotations[admittedMemberAnnotation] = string(encoded)
						}
					}

					got, diagnostics, err := reconcileMembers([]corev1.Node{node}, pods, memberOwnership(t, testDaemonSetUID), accepted, 7443)
					require.NoError(t, err)
					require.Len(t, got, 1)
					require.Equal(t, tc.want, got[testNodeUID].Site)

					if mode == "cold" {
						require.Empty(t, diagnostics)
					} else {
						require.Len(t, diagnostics, 2)

						previous.Site = tc.want
						require.Equal(t, previous, got[testNodeUID])
					}
				})
			}
		})
	}
}

func TestNodeChangesSiteLabels(t *testing.T) {
	for _, key := range []string{machinav1.MachineSiteLabelKey, "net.unbounded-cloud.io/site"} {
		for _, values := range [][2]string{{"", "site-a"}, {"site-a", "site-b"}, {"site-a", ""}} {
			old, next := memberNode(), memberNode()
			if values[0] != "" {
				old.Labels = map[string]string{key: values[0]}
			}

			if values[1] != "" {
				next.Labels = map[string]string{key: values[1]}
			}

			require.Equal(t, key == machinav1.MachineSiteLabelKey, nodeChanges().Update(event.UpdateEvent{ObjectOld: &old, ObjectNew: &next}), "%s %v", key, values)
		}
	}

	old, next := memberNode(), memberNode()
	next.Labels = map[string]string{"unrelated": "site-a"}
	require.False(t, nodeChanges().Update(event.UpdateEvent{ObjectOld: &old, ObjectNew: &next}))
}

func TestTopologySiteChangesPersistAcrossRestart(t *testing.T) {
	node := memberNode()
	node.Labels = map[string]string{machinav1.MachineSiteLabelKey: "site-a"}
	node.Annotations = map[string]string{wire.RailsAnnotation: `[{"rail":0,"fabric":"fabric-a"}]`}
	pod := memberPod("a", 1, "192.0.2.1")
	r := initializedTopology(t, &node, &pod, &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: DataplaneDaemonSetName, Namespace: "racer", UID: testDaemonSetUID}})
	first := reconcileTopology(t, r, t.Context())
	base, err := wire.DecodePublication(strings.NewReader(first.Encoding()))
	require.NoError(t, err)
	require.Equal(t, "site-a", base.Members[0].Site)

	for _, site := range []string{"site-b", ""} {
		require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(&node), &node))

		node.Labels = nil
		if site != "" {
			node.Labels = map[string]string{machinav1.MachineSiteLabelKey: site}
		}

		node.Annotations[wire.RailsAnnotation] = "malformed"
		require.NoError(t, r.Update(t.Context(), &node))
		// Drop all in-memory history before processing the changed boundary.
		r = Assemble(r.Config, r.Client, r.APIReader).Topology
		committed := reconcileTopology(t, r, t.Context())
		next, err := wire.DecodePublication(strings.NewReader(committed.Encoding()))
		require.NoError(t, err)
		require.Equal(t, site, next.Members[0].Site)
		require.Equal(t, base.Members[0].Rails, next.Members[0].Rails)
		require.Equal(t, base.Sequence+1, next.Sequence)
		require.Equal(t, base.MembershipVersion+1, next.MembershipVersion)
		oldContent, oldMembership, err := wire.ContentHashes(base)
		require.NoError(t, err)
		content, membership, err := wire.ContentHashes(next)
		require.NoError(t, err)
		require.NotEqual(t, oldContent, content)
		require.NotEqual(t, oldMembership, membership)

		delta, err := wire.EncodeDelta(base, next)
		require.NoError(t, err)
		applied, err := wire.ApplyDelta(base, strings.NewReader(string(delta)))
		require.NoError(t, err)
		require.Equal(t, next, applied)
		require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(&node), &node))

		var saved wire.Member
		require.NoError(t, json.Unmarshal([]byte(node.Annotations[admittedMemberAnnotation]), &saved))
		require.Equal(t, next.Members[0], saved)

		r = Assemble(r.Config, r.Client, r.APIReader).Topology
		require.Equal(t, committed.Encoding(), reconcileTopology(t, r, t.Context()).Encoding())

		base = next
	}
}
