// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"

	statusv1alpha1 "github.com/Azure/unbounded/internal/net/status/v1alpha1"
)

// WatchMetadata atomically returns current metadata and a change notification.
// No payload escapes into a waiter, subscription, or callback closure.
func (m *nodeDetailRequests) WatchMetadata(nodeName, requestID string) (statusv1alpha1.NodeDetailResult, <-chan struct{}) {
	m.mu.Lock()
	defer m.mu.Unlock()

	result := m.resultForIDLocked(nodeName, requestID)
	if result.Details != nil {
		metadata := *result.Details
		metadata.Status = nil
		result.Details = &metadata
	}

	return result, m.updates
}

// Wait supports the legacy synchronous per-node route without starting another
// pull. Canceling one waiter never cancels a shared request.
func (m *nodeDetailRequests) Wait(ctx context.Context, nodeName, requestID string) statusv1alpha1.NodeDetailResult {
	for {
		result, changed := m.WatchMetadata(nodeName, requestID)
		if result.State != statusv1alpha1.NodeDetailPending {
			return m.Result(nodeName, requestID)
		}

		select {
		case <-ctx.Done():
			return detailRequestFailure(nodeName, requestID, statusv1alpha1.NodeDetailRetryable, ctx.Err().Error())
		case <-changed:
		}
	}
}
