// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"

	statusv1alpha1 "github.com/Azure/unbounded/internal/net/status/v1alpha1"
)

// Hooks must honor cancellation. Resolve reads the current informer identity;
// Dispatch returns true only when an active WebSocket accepted the command.
type nodeDetailRequestHooks struct {
	Resolve  func(string) (types.UID, error)
	Dispatch func(context.Context, string, statusv1alpha1.DetailRequest) (bool, error)
	Pull     func(context.Context, string) (*NodeStatusResponse, error)
}

// A request owns metadata and cancellation only, never a result payload.
type nodeDetailRequest struct {
	nodeName    string
	uid         types.UID
	command     statusv1alpha1.DetailRequest
	state       statusv1alpha1.NodeDetailState
	message     string
	wakeAt      time.Time
	poll        bool
	cancel      context.CancelFunc
	ctx         context.Context
	dispatching bool
	retry       bool
}

type nodeDetailRequests struct {
	mu       sync.Mutex
	ctx      context.Context
	cancel   context.CancelFunc
	done     chan struct{}
	changed  chan struct{}
	workers  sync.WaitGroup
	closed   bool
	timeout  time.Duration
	cache    *nodeDetailCache
	hooks    nodeDetailRequestHooks
	requests map[string]*nodeDetailRequest
	active   map[string]*nodeDetailRequest
}

// newNodeDetailRequests takes exclusive lifecycle ownership of cache. All
// snapshots enter through Complete or ObserveLegacy so their UID binding is known.
func newNodeDetailRequests(ctx context.Context, cache *nodeDetailCache, timeout time.Duration, hooks nodeDetailRequestHooks) (*nodeDetailRequests, error) {
	if cache == nil || timeout <= 0 || hooks.Resolve == nil {
		return nil, errors.New("detail requests require a cache, positive timeout, and node resolver")
	}

	ctx, cancel := context.WithCancel(ctx)
	m := &nodeDetailRequests{
		ctx: ctx, cancel: cancel, done: make(chan struct{}), changed: make(chan struct{}, 1),
		timeout: timeout, cache: cache, hooks: hooks,
		requests: make(map[string]*nodeDetailRequest), active: make(map[string]*nodeDetailRequest),
	}
	cache.Clear()

	go m.run()

	return m, nil
}

// Close cancels dispatches, clears leader-local state, and waits for all workers.
func (m *nodeDetailRequests) Close() {
	m.cancel()
	<-m.done
}

func (m *nodeDetailRequests) Request(nodeName string, forceRefresh bool) statusv1alpha1.NodeDetailResult {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.ctx.Err() != nil || m.closed {
		return detailRequestFailure(nodeName, "", statusv1alpha1.NodeDetailRetryable, "detail request leader is unavailable")
	}

	m.expireLocked(time.Now())

	uid, err := m.hooks.Resolve(nodeName)
	if err != nil || uid == "" {
		return detailRequestFailure(nodeName, "", statusv1alpha1.NodeDetailUnavailable, "node identity is unavailable")
	}

	for _, request := range m.requests {
		if request.nodeName == nodeName && request.uid != uid {
			m.invalidateLocked(request)
		}
	}

	if !forceRefresh {
		if snapshot, ok := m.cache.Get(nodeName); ok {
			if request := m.requests[snapshot.RequestID]; request != nil && request.uid == uid {
				return m.resultLocked(request)
			}
		}
	}

	if request := m.active[nodeName]; request != nil {
		return m.resultLocked(request)
	}

	now := time.Now()
	request := &nodeDetailRequest{
		nodeName: nodeName, uid: uid, state: statusv1alpha1.NodeDetailPending,
		command: statusv1alpha1.DetailRequest{RequestID: rand.Text(), Deadline: now.Add(m.timeout)},
		wakeAt:  now.Add(m.timeout),
	}
	ctx, cancel := context.WithDeadline(m.ctx, request.command.Deadline)
	request.cancel = cancel
	request.ctx = ctx
	m.requests[request.command.RequestID] = request
	m.active[nodeName] = request
	m.notify()
	m.startDispatchLocked(request)

	return m.resultLocked(request)
}

func (m *nodeDetailRequests) Result(nodeName, requestID string) statusv1alpha1.NodeDetailResult {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.ctx.Err() != nil || m.closed {
		return detailRequestFailure(nodeName, requestID, statusv1alpha1.NodeDetailRetryable, "detail request leader is unavailable")
	}

	m.expireLocked(time.Now())

	request := m.requests[requestID]
	if request == nil || request.nodeName != nodeName {
		return detailRequestFailure(nodeName, requestID, statusv1alpha1.NodeDetailRetryable, "request is no longer known; retry on the current leader")
	}

	if uid, err := m.hooks.Resolve(nodeName); err != nil || uid != request.uid {
		m.invalidateLocked(request)
	}

	return m.resultLocked(request)
}

// Complete is idempotent while a completed request is retained. It rejects
// mismatched, late, and expired replies without replacing data or renewing TTL.
func (m *nodeDetailRequests) Complete(nodeName, requestID string, status *NodeStatusResponse) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.expireLocked(time.Now())

	request := m.requests[requestID]
	if m.ctx.Err() != nil || m.closed || request == nil || request.nodeName != nodeName {
		return errors.New("detail request is no longer available")
	}

	if uid, err := m.hooks.Resolve(nodeName); err != nil || uid != request.uid {
		m.invalidateLocked(request)

		return errors.New("detail request node was deleted or replaced")
	}

	if status == nil || status.NodeInfo.Name != nodeName || status.FetchError != "" {
		return errors.New("detail response has missing data, a fetch error, or a mismatched node name")
	}

	if request.state == statusv1alpha1.NodeDetailComplete {
		return nil
	}

	if request.state != statusv1alpha1.NodeDetailPending {
		return errors.New("detail request is no longer pending")
	}

	snapshot, err := m.cache.Store(nodeName, requestID, status.Timestamp, status)
	if err != nil {
		return err
	}

	request.state = statusv1alpha1.NodeDetailComplete
	request.message = ""
	request.poll = false
	request.wakeAt = snapshot.ExpiresAt
	request.cancel()
	delete(m.active, nodeName)
	m.notify()

	return nil
}

// Pending exposes only a failed-pull fallback command, without refreshing its
// deadline. Returning it repeatedly is safe until a valid reply completes it.
func (m *nodeDetailRequests) Pending(nodeName string) (statusv1alpha1.DetailRequest, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.expireLocked(time.Now())

	request := m.active[nodeName]
	if m.ctx.Err() != nil || request == nil || !request.poll {
		return statusv1alpha1.DetailRequest{}, false
	}

	if uid, err := m.hooks.Resolve(nodeName); err != nil || uid != request.uid {
		m.invalidateLocked(request)

		return statusv1alpha1.DetailRequest{}, false
	}

	return request.command, true
}

// InvalidateNode handles deletion/replacement of a specific UID. A delayed old
// informer event cannot cancel a request for a newer node with the same name.
func (m *nodeDetailRequests) InvalidateNode(nodeName string, uid types.UID) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, request := range m.requests {
		if request.nodeName == nodeName && request.uid == uid {
			m.invalidateLocked(request)
		}
	}
}

func (m *nodeDetailRequests) invalidateLocked(request *nodeDetailRequest) {
	if request.state == statusv1alpha1.NodeDetailUnavailable {
		return
	}

	request.cancel()
	request.state = statusv1alpha1.NodeDetailUnavailable
	request.message = "node was deleted or replaced"
	request.poll = false
	request.wakeAt = time.Now().Add(m.timeout)

	if m.active[request.nodeName] == request {
		delete(m.active, request.nodeName)
	}

	if snapshot, ok := m.cache.Get(request.nodeName); ok && snapshot.RequestID == request.command.RequestID {
		m.cache.Delete(request.nodeName)
	}

	m.notify()
}

func (m *nodeDetailRequests) resultLocked(request *nodeDetailRequest) statusv1alpha1.NodeDetailResult {
	result := statusv1alpha1.NodeDetailResult{
		NodeName: request.nodeName, RequestID: request.command.RequestID, Deadline: request.command.Deadline,
		State: request.state, Error: request.message,
	}
	if request.state == statusv1alpha1.NodeDetailComplete {
		if snapshot, ok := m.cache.Get(request.nodeName); ok && snapshot.RequestID == request.command.RequestID {
			details := snapshot.NodeDetailSnapshot
			result.Details = &details
		} else {
			result.State = statusv1alpha1.NodeDetailExpired
			result.Error = "details expired or were replaced"
		}
	}

	return result
}

func detailRequestFailure(nodeName, requestID string, state statusv1alpha1.NodeDetailState, message string) statusv1alpha1.NodeDetailResult {
	return statusv1alpha1.NodeDetailResult{NodeName: nodeName, RequestID: requestID, State: state, Error: message}
}

func (m *nodeDetailRequests) dispatch(ctx context.Context, nodeName string, command statusv1alpha1.DetailRequest) {
	if m.hooks.Dispatch != nil {
		if sent, err := m.hooks.Dispatch(ctx, nodeName, command); sent && err == nil {
			return
		}
	}

	err := errors.New("node HTTP detail pull is unavailable")

	if m.hooks.Pull != nil && ctx.Err() == nil {
		pullCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		status, pullErr := m.hooks.Pull(pullCtx, nodeName)

		cancel()

		err = pullErr
		if err == nil {
			err = m.Complete(nodeName, command.RequestID, status)
		}

		if err == nil {
			return
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	m.expireLocked(time.Now())

	if request := m.active[nodeName]; request != nil && request.command.RequestID == command.RequestID && m.ctx.Err() == nil {
		request.poll = true
		request.message = fmt.Sprintf("HTTP detail pull failed; waiting for status POST: %v", err)
	}
}

func (m *nodeDetailRequests) expireLocked(now time.Time) time.Time {
	var next time.Time

	for id, request := range m.requests {
		if !now.Before(request.wakeAt) {
			if request.state == statusv1alpha1.NodeDetailPending || request.state == statusv1alpha1.NodeDetailComplete {
				request.cancel()
				request.state = statusv1alpha1.NodeDetailExpired
				request.message = "detail request or snapshot expired"
				request.poll = false
				request.wakeAt = request.wakeAt.Add(m.timeout)

				if m.active[request.nodeName] == request {
					delete(m.active, request.nodeName)
				}
			}

			if !now.Before(request.wakeAt) {
				delete(m.requests, id)

				continue
			}
		}

		if next.IsZero() || request.wakeAt.Before(next) {
			next = request.wakeAt
		}
	}

	return next
}

func (m *nodeDetailRequests) notify() {
	select {
	case m.changed <- struct{}{}:
	default:
	}
}

func (m *nodeDetailRequests) run() {
	cacheDone := make(chan struct{})
	go func() {
		defer close(cacheDone)

		if err := m.cache.Run(m.ctx); err != nil {
			klog.Errorf("Node detail cache expiry loop failed: %v", err)
			m.cancel()
		}
	}()

	for m.ctx.Err() == nil {
		m.mu.Lock()
		next := m.expireLocked(time.Now())
		m.mu.Unlock()

		var (
			timer  *time.Timer
			timerC <-chan time.Time
		)

		if !next.IsZero() {
			timer = time.NewTimer(time.Until(next))
			timerC = timer.C
		}

		select {
		case <-m.ctx.Done():
		case <-m.changed:
		case <-timerC:
		}

		if timer != nil {
			timer.Stop()
		}
	}

	m.mu.Lock()
	m.closed = true

	for _, request := range m.requests {
		request.cancel()
	}

	clear(m.requests)
	clear(m.active)
	m.cache.Clear()
	m.mu.Unlock()
	m.workers.Wait()
	<-cacheDone
	close(m.done)
}
