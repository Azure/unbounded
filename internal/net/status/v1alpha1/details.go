// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import "time"

type NodeDetailState string

const (
	NodeDetailPending     NodeDetailState = "pending"
	NodeDetailComplete    NodeDetailState = "complete"
	NodeDetailExpired     NodeDetailState = "expired"
	NodeDetailUnavailable NodeDetailState = "unavailable"
	NodeDetailRetryable   NodeDetailState = "retryable"
)

// NodeDetailSnapshot is a single expiring, immutable diagnostic result.
type NodeDetailSnapshot struct {
	NodeName    string              `json:"nodeName"`
	RequestID   string              `json:"requestId"`
	CollectedAt time.Time           `json:"collectedAt"`
	ReceivedAt  time.Time           `json:"receivedAt"`
	ExpiresAt   time.Time           `json:"expiresAt"`
	Status      *NodeStatusResponse `json:"status"`
}

// NodeDetailResult describes a leader-local request. Details are absent unless
// the corresponding snapshot is still available in the detail cache.
type NodeDetailResult struct {
	State     NodeDetailState     `json:"state"`
	NodeName  string              `json:"nodeName"`
	RequestID string              `json:"requestId,omitempty"`
	Deadline  time.Time           `json:"deadline,omitempty"`
	Error     string              `json:"error,omitempty"`
	Details   *NodeDetailSnapshot `json:"details,omitempty"`
}
