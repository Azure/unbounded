// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package members - self-announce: write this agent's libp2p peer.ID,
// multiaddrs and transfer endpoint into its own Pod's annotations so
// other agents can discover the libp2p identity without
// operator-supplied bootstrap config. See the design doc (libp2p bootstrap) and
// the design doc (membership view).
package members

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

// SelfAnnouncement is the data each agent publishes onto its own Pod.
// Empty fields produce empty annotations (which clears any stale value
// from a previous incarnation of the pod IP).
type SelfAnnouncement struct {
	// PeerID is the libp2p peer.ID string form.
	PeerID string

	// P2PAddrs is the agent's libp2p listen multiaddrs. Each entry is
	// a full /ip4/.../tcp/<port>/p2p/<peerID> multiaddr.
	P2PAddrs []string

	// TransferAddr is the host:port for the HTTP/2 transfer endpoint
	// reachable from peer pods. When empty, peers reconstruct
	// "podIP:TransferPort" from members.Options.TransferPort instead.
	TransferAddr string
}

// AnnounceSelf writes the announcement into the named Pod's annotations
// via a JSON-merge-patch. PodName MUST identify the agent's own pod
// (typically sourced from the Downward API as $POD_NAME); Namespace
// defaults to the namespace the informer was scoped to.
//
// The patch is a strict overwrite: any prior gantry.io/* annotation
// values are replaced. Other annotations on the pod are left untouched.
//
// AnnounceSelf requires `pods` `patch` verb on the agent's own
// namespace; see deploy/gantry/serviceaccount.yaml.
func (m *Manager) AnnounceSelf(ctx context.Context, podName string, ann SelfAnnouncement) error {
	return AnnounceSelf(ctx, m.clientset, m.namespace, podName, ann)
}

// AnnounceSelf publishes a peer endpoint without requiring pod or node
// informers. Upgraded agents use it only so old membership-based peers can
// discover them during a rolling deployment.
func AnnounceSelf(ctx context.Context, clientset kubernetes.Interface, namespace, podName string, ann SelfAnnouncement) error {
	if podName == "" {
		return errors.New("members: AnnounceSelf requires podName (set GANTRY_POD_NAME via Downward API)")
	}

	if clientset == nil {
		return errors.New("members: AnnounceSelf called before clientset wired")
	}

	if namespace == "" {
		return errors.New("members: AnnounceSelf requires Options.Namespace (cluster-wide informer cannot self-patch)")
	}

	annotations := map[string]string{
		AnnotationPeerID:       ann.PeerID,
		AnnotationP2PAddrs:     strings.Join(ann.P2PAddrs, ","),
		AnnotationTransferAddr: ann.TransferAddr,
	}
	// JSON-merge-patch: keys with empty-string values explicitly
	// overwrite (do NOT use `null` here - that would delete the key,
	// which is also acceptable but less aligned with the other
	// values being string-typed).
	patch := map[string]any{
		"metadata": map[string]any{
			"annotations": annotations,
		},
	}

	body, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("members: marshal self-announce patch: %w", err)
	}

	_, err = clientset.CoreV1().Pods(namespace).Patch(
		ctx, podName, types.MergePatchType, body, metav1.PatchOptions{},
	)
	if err != nil {
		return fmt.Errorf("members: patch pod %s/%s annotations: %w", namespace, podName, err)
	}

	return nil
}
