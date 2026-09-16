// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"time"

	statusv1alpha1 "github.com/Azure/unbounded/internal/net/status/v1alpha1"
)

// Fail records a correlated collection failure without deleting an older,
// still-valid snapshot that a viewer may display during a failed refresh.
func (m *nodeDetailRequests) Fail(nodeName, requestID, reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.expireLocked(time.Now())

	request := m.requests[requestID]
	if reason == "" || m.ctx.Err() != nil || m.closed || request == nil || request.nodeName != nodeName {
		return errors.New("detail failure does not match an available request")
	}

	if uid, err := m.hooks.Resolve(nodeName); err != nil || uid != request.uid {
		m.invalidateLocked(request)
		return errors.New("detail request node was deleted or replaced")
	}

	if request.state == statusv1alpha1.NodeDetailComplete ||
		(request.state == statusv1alpha1.NodeDetailUnavailable && request.message == reason) {
		return nil
	}

	if request.state != statusv1alpha1.NodeDetailPending {
		return errors.New("detail request is no longer pending")
	}

	request.cancel()
	request.state = statusv1alpha1.NodeDetailUnavailable
	request.message = reason
	request.poll = false
	request.wakeAt = time.Now().Add(m.timeout)
	delete(m.active, nodeName)
	m.notify()

	return nil
}

func handleNodeDetailResponse(health *healthState, nodeName, requestID string, status *NodeStatusResponse, failure string) NodeStatusPushAck {
	ack := NodeStatusPushAck{Status: "error", DetailRequestID: requestID, SummarySupported: true}

	manager := health.getDetailRequests()
	if manager == nil || requestID == "" {
		ack.Reason = "detail request leader or request identity is unavailable"
		return ack
	}

	if failure != "" && status != nil {
		ack.Reason = "detail response cannot contain both data and a collection error"
		return ack
	}

	var err error
	if failure != "" {
		err = manager.Fail(nodeName, requestID, failure)
	} else {
		err = manager.Complete(nodeName, requestID, status)
	}

	if err != nil {
		ack.Reason = err.Error()
		return ack
	}

	ack.Status = "ok"

	return ack
}
