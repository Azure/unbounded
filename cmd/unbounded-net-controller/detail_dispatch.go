// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"time"

	statusv1alpha1 "github.com/Azure/unbounded/internal/net/status/v1alpha1"
)

type nodeWSConnection struct {
	cancel context.CancelFunc
	send   func(context.Context, statusv1alpha1.DetailRequest) error
}

func (h *healthState) setNodeWSDetailSender(nodeName string, connection *nodeWSConnection, send func(context.Context, statusv1alpha1.DetailRequest) error) {
	h.nodeWSMu.Lock()

	ready := false
	if connection != nil && h.nodeWSRegistry[nodeName] == connection {
		ready = connection.send == nil
		connection.send = send
	}
	h.nodeWSMu.Unlock()

	if ready {
		h.retryNodeDetails(nodeName)
	}
}

func (h *healthState) dispatchNodeDetail(ctx context.Context, nodeName string, command statusv1alpha1.DetailRequest) (bool, error) {
	h.nodeWSMu.Lock()

	var send func(context.Context, statusv1alpha1.DetailRequest) error
	if connection := h.nodeWSRegistry[nodeName]; connection != nil {
		send = connection.send
	}
	h.nodeWSMu.Unlock()

	if send == nil {
		return false, nil
	}

	writeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	err := send(writeCtx, command)

	return err == nil, err
}

func (h *healthState) retryNodeDetails(nodeName string) {
	if manager := h.getDetailRequests(); manager != nil {
		manager.Retry(nodeName)
	}
}

// Retry wakes an existing request after a transport change, retaining its ID,
// deadline, and one-dispatch-at-a-time ownership.
func (m *nodeDetailRequests) Retry(nodeName string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.expireLocked(time.Now())

	if request := m.active[nodeName]; request != nil && m.ctx.Err() == nil && !m.closed {
		request.retry = true
		if !request.dispatching {
			m.startDispatchLocked(request)
		}
	}
}

func (m *nodeDetailRequests) startDispatchLocked(request *nodeDetailRequest) {
	request.dispatching = true
	request.retry = false
	request.poll = false

	m.workers.Go(func() {
		m.dispatch(request.ctx, request.nodeName, request.command)

		m.mu.Lock()
		defer m.mu.Unlock()

		request.dispatching = false
		if m.active[request.nodeName] == request && request.retry && m.ctx.Err() == nil && !m.closed {
			m.startDispatchLocked(request)
		}
	})
}
