// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"time"

	statusproto "github.com/Azure/unbounded/internal/net/status/proto"
	statusv1alpha1 "github.com/Azure/unbounded/internal/net/status/v1alpha1"
)

func protoToNodeOverview(msg *statusproto.NodeStatusOverview) statusv1alpha1.NodeStatusOverview {
	overview := statusv1alpha1.NodeStatusOverview{
		HealthCheck: protoToHealthCheckStatus(msg.HealthCheck),
		NodeErrors:  protoToNodeErrors(msg.NodeErrors),
		FetchError:  msg.FetchError, StatusSource: msg.StatusSource,
		NodePodInfo: protoToNodePodInfo(msg.NodePodInfo),
		PeerCount:   int(msg.PeerCount), HealthyPeers: int(msg.HealthyPeers),
		RouteCount: int(msg.RouteCount), RouteMismatch: msg.RouteMismatch,
	}
	if msg.NodeInfo != nil {
		overview.NodeInfo = protoToNodeInfo(msg.NodeInfo)
	}

	if msg.TimestampUnixNs != 0 {
		overview.Timestamp = time.Unix(0, msg.TimestampUnixNs)
	}

	if msg.LastPushTimeUnixNs != 0 {
		lastPush := time.Unix(0, msg.LastPushTimeUnixNs)
		overview.LastPushTime = &lastPush
	}

	return overview
}
