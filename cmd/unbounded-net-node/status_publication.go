// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"reflect"
	"time"

	statusproto "github.com/Azure/unbounded/internal/net/status/proto"
	statusv1alpha1 "github.com/Azure/unbounded/internal/net/status/v1alpha1"
)

const nodeErrorSummaryUnsupported = "status-summary-unsupported"

func collectPublication(health *nodeHealthState, cfg *config, previous *NodeStatusResponse, force bool, revision uint64) (*statusproto.NodeStatusMessage, *NodeStatusResponse) {
	if cfg.StatusDetailMode == "summary" {
		summary := health.getSummarySnapshot()

		return &statusproto.NodeStatusMessage{
			Type: statusv1alpha1.NodeStatusSummaryType, NodeName: summary.NodeInfo.Name,
			BaseRevision: revision, Summary: nodeSummaryToProto(summary),
		}, nil
	}

	full := health.getStatusSnapshot()

	msg := &statusproto.NodeStatusMessage{Type: "node_status_full", NodeName: full.NodeInfo.Name}
	if cfg.StatusPushDelta && !force {
		msg.Delta = typedStatusDelta(previous, full, false, true)
		if msg.Delta != nil {
			msg.Type, msg.BaseRevision = "node_status_delta", revision
		}
	}

	if msg.Delta == nil {
		msg.Status = nodeStatusToProto(full)
	}

	return msg, full
}

func publicationNodeErrors(errors []NodeError) []NodeError {
	result := make([]NodeError, 0, len(errors))
	for _, err := range errors {
		switch err.Type {
		case nodeErrorTypeDirectPush, nodeErrorTypeDirectWebSocket, nodeErrorTypeFallbackPush, nodeErrorTypeFallbackWS:
			continue
		default:
			result = append(result, err)
		}
	}

	return result
}

func equalPublicationSummaries(a, b *NodeStatusOverview) bool {
	if a == nil || b == nil {
		return a == b
	}

	normalize := func(summary *NodeStatusOverview) NodeStatusOverview {
		result := *summary
		result.Timestamp = time.Time{}

		if summary.HealthCheck != nil {
			health := *summary.HealthCheck
			health.CheckedAt = time.Time{}
			result.HealthCheck = &health
		}

		return result
	}

	return reflect.DeepEqual(normalize(a), normalize(b))
}
