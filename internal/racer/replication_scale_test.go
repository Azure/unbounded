// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/Azure/unbounded/internal/racer/members"
	"github.com/Azure/unbounded/internal/racer/wire"
)

// These are real HTTPS protocol clients, not Rust processes or in-memory Wait
// calls. Kubernetes authority is fake. Never interpret this as API capacity.
func replicationSmokePublish(t *testing.T, ctx context.Context, r *TopologyReconciler, accepted AcceptedMembers) *CommittedPublication {
	t.Helper()

	_, err := r.authority.PublishTopology(ctx, func(context.Context) (TopologyObservation, error) {
		nodes := corev1.NodeList{}

		for id, member := range accepted {
			encoded, err := json.Marshal(member)
			if err != nil {
				return TopologyObservation{}, err
			}

			nodes.Items = append(nodes.Items, corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: string(id), UID: types.UID(id), Annotations: map[string]string{admittedMemberAnnotation: string(encoded), wire.SharesAnnotation: strconv.FormatUint(uint64(member.Shares), 10)}}})
		}

		return TopologyObservation{Nodes: nodes, Input: members.Input{Nodes: nodes.Items, PeerPort: r.Config.PeerPort}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	return capturePublication(t, r.authority)
}
