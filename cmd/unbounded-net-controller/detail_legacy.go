// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/rand"
	"errors"
	"time"

	statusv1alpha1 "github.com/Azure/unbounded/internal/net/status/v1alpha1"
)

// ObserveLegacy stores actual legacy details, not a summary or source update.
// A continuous stream reuses the completed cache association rather than making
// a request record per publication. It never fulfills an unrelated pending
// request. A non-nil base requires the exact, unexpired delta base at commit.
func (m *nodeDetailRequests) ObserveLegacy(nodeName string, status *NodeStatusResponse, revision uint64, identity *peerIdentityDigest, base *NodeStatusResponse) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.expireLocked(time.Now())

	if m.ctx.Err() != nil || m.closed {
		return errors.New("detail request leader is unavailable")
	}

	if status == nil || status.NodeInfo.Name != nodeName || revision == 0 {
		return errors.New("legacy details require a matching node name and wire revision")
	}

	uid, err := m.hooks.Resolve(nodeName)
	if err != nil || uid == "" {
		m.forgetLocked(nodeName)

		return errors.New("legacy detail node identity is unavailable")
	}

	var request *nodeDetailRequest

	if snapshot, ok := m.cache.Get(nodeName); ok {
		existing := m.requests[snapshot.RequestID]
		if existing != nil && existing.uid == uid && existing.state == statusv1alpha1.NodeDetailComplete {
			request = existing
		} else if existing != nil && existing.uid != uid {
			m.invalidateLocked(existing)
		}
	}

	if request == nil {
		request = &nodeDetailRequest{
			nodeName: nodeName, uid: uid, state: statusv1alpha1.NodeDetailComplete,
			command: statusv1alpha1.DetailRequest{RequestID: rand.Text()},
			cancel:  func() {},
		}
	}

	snapshot, err := m.cache.store(nodeName, request.command.RequestID, status.Timestamp, status, revision, identity, base)
	if err != nil {
		return err
	}

	request.wakeAt = snapshot.ExpiresAt
	m.requests[request.command.RequestID] = request
	m.notify()

	return nil
}

// LegacyBase returns only the current wire base. An on-demand snapshot cannot
// substitute for it, even if its payload happens to look identical.
func (m *nodeDetailRequests) LegacyBase(nodeName string, revision uint64) (*NodeStatusResponse, *peerIdentityDigest, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.ctx.Err() != nil || m.closed {
		return nil, nil, false
	}

	snapshot, ok := m.cache.Get(nodeName)
	if !ok || snapshot.legacyRevision == 0 || snapshot.legacyRevision != revision {
		return nil, nil, false
	}

	request := m.requests[snapshot.RequestID]

	uid, err := m.hooks.Resolve(nodeName)
	if request == nil || err != nil || request.uid != uid {
		m.forgetLocked(nodeName)

		return nil, nil, false
	}

	return snapshot.Status, snapshot.peerIdentity, true
}

// Forget drops the current node's request state and cache ownership.
func (m *nodeDetailRequests) Forget(nodeName string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.forgetLocked(nodeName)
}

func (m *nodeDetailRequests) forgetLocked(nodeName string) {
	for _, request := range m.requests {
		if request.nodeName == nodeName {
			m.invalidateLocked(request)
		}
	}

	m.cache.Delete(nodeName)
}

// CompleteFailure terminates only a correlated live request. A failed refresh
// leaves an older, still-valid cached result alone.
func (m *nodeDetailRequests) CompleteFailure(nodeName, requestID, message string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.expireLocked(time.Now())

	request := m.requests[requestID]
	if m.ctx.Err() != nil || m.closed || request == nil || request.nodeName != nodeName || message == "" {
		return errors.New("detail failure does not match an available request")
	}

	if uid, err := m.hooks.Resolve(nodeName); err != nil || uid != request.uid {
		m.invalidateLocked(request)

		return errors.New("detail failure node was deleted or replaced")
	}

	if request.state == statusv1alpha1.NodeDetailComplete || request.state == statusv1alpha1.NodeDetailUnavailable {
		return nil
	}

	if request.state != statusv1alpha1.NodeDetailPending {
		return errors.New("detail request is no longer pending")
	}

	request.cancel()
	request.state = statusv1alpha1.NodeDetailUnavailable
	request.message = message
	request.poll = false
	request.wakeAt = time.Now().Add(m.timeout)
	delete(m.active, nodeName)
	m.notify()

	return nil
}
