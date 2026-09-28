// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"
	"k8s.io/klog/v2"

	netstatus "github.com/Azure/unbounded/internal/net/status"
	statusproto "github.com/Azure/unbounded/internal/net/status/proto"
	statusv1alpha1 "github.com/Azure/unbounded/internal/net/status/v1alpha1"
)

const (
	nodeDetailFrameLimit    = 2 * 1024 * 1024
	nodeDetailRetryInterval = time.Second
)

type nodeDetailReply struct {
	request    statusv1alpha1.DetailRequest
	payload    []byte
	sending    bool
	collecting bool
	ready      bool
	done       bool
	retryAt    time.Time
}

// One state spans both publishers and reconnects. Successful ACKs retain only
// request identity/deadline markers; no routine publisher owns detail snapshots.
type nodeDetailState struct {
	mu           sync.Mutex
	replies      map[string]*nodeDetailReply
	wsWake       chan struct{}
	httpWake     chan struct{}
	collectWake  chan struct{}
	workerCancel context.CancelFunc
	workerDone   chan struct{}
}

func (h *nodeHealthState) detailState() *nodeDetailState {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.details == nil {
		h.details = &nodeDetailState{
			replies: make(map[string]*nodeDetailReply),
			wsWake:  make(chan struct{}, 1), httpWake: make(chan struct{}, 1),
			collectWake: make(chan struct{}, 1),
		}
	}

	return h.details
}

func (s *nodeDetailState) signalCollection() {
	select {
	case s.collectWake <- struct{}{}:
	default:
	}
}

func (s *nodeDetailState) start(ctx context.Context, nodeName string, collect func() *NodeStatusResponse) <-chan struct{} {
	s.mu.Lock()
	if s.workerDone != nil {
		done := s.workerDone
		s.mu.Unlock()

		return done
	}

	workerCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	s.workerCancel = cancel
	s.workerDone = done
	s.mu.Unlock()

	// One worker bounds a slow kernel-backed collection to one goroutine while
	// routine HTTP and WebSocket publishers remain responsive.
	go func() {
		defer close(done)

		s.run(workerCtx, nodeName, collect)
	}()

	s.signalCollection()

	return done
}

func (s *nodeDetailState) stop() {
	s.mu.Lock()
	cancel := s.workerCancel
	clear(s.replies)
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}

	s.signalCollection()
}

func (s *nodeDetailState) wake() {
	for _, ch := range []chan struct{}{s.wsWake, s.httpWake} {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

type nodeDetailCollection struct {
	id       string
	deadline time.Time
}

func (s *nodeDetailState) nextCollection(now time.Time) *nodeDetailCollection {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.expireLocked(now)

	for _, reply := range s.replies {
		if !reply.done && !reply.collecting && !reply.ready {
			reply.collecting = true

			return &nodeDetailCollection{id: reply.request.RequestID, deadline: reply.request.Deadline}
		}
	}

	return nil
}

func (s *nodeDetailState) completeCollection(collection *nodeDetailCollection, payload []byte, now time.Time) {
	s.mu.Lock()

	reply := s.replies[collection.id]
	if reply == nil || reply.request.Deadline != collection.deadline {
		s.mu.Unlock()

		return
	}

	reply.collecting = false
	if !reply.request.Deadline.After(now) {
		delete(s.replies, collection.id)
		s.mu.Unlock()

		return
	}

	if reply.done {
		s.mu.Unlock()

		return
	}

	reply.payload = payload
	reply.ready = true
	s.mu.Unlock()
	s.wake()
}

func (s *nodeDetailState) run(ctx context.Context, nodeName string, collect func() *NodeStatusResponse) {
	for {
		collection := s.nextCollection(time.Now())
		if collection == nil {
			select {
			case <-ctx.Done():
				return
			case <-s.collectWake:
				continue
			}
		}

		payload := collectDetailPayload(nodeName, collection.id, collect)
		s.completeCollection(collection, payload, time.Now())

		if ctx.Err() != nil {
			return
		}
	}
}

func (s *nodeDetailState) expireLocked(now time.Time) {
	for id, reply := range s.replies {
		if !reply.request.Deadline.After(now) {
			delete(s.replies, id)
		}
	}
}

func (s *nodeDetailState) enqueue(request *statusv1alpha1.DetailRequest, now time.Time) error {
	if err := netstatus.ValidateDetailRequest(request, now); err != nil {
		return err
	}

	s.mu.Lock()
	s.expireLocked(now)

	if _, exists := s.replies[request.RequestID]; !exists {
		s.replies[request.RequestID] = &nodeDetailReply{request: *request}
		time.AfterFunc(time.Until(request.Deadline), func() {
			s.mu.Lock()
			defer s.mu.Unlock()

			s.expireLocked(time.Now())
		})
	}
	s.mu.Unlock()
	s.signalCollection()

	return nil
}

func (s *nodeDetailState) receive(ack *statusv1alpha1.NodeStatusAck) {
	if ack == nil {
		return
	}

	if ack.DetailRequestID != "" && ack.Status != "ok" {
		klog.V(2).Infof("Detail reply %q not acknowledged: status=%s reason=%s", ack.DetailRequestID, ack.Status, ack.Reason)
	}

	s.acknowledge(ack)

	if ack.DetailRequest != nil {
		if err := s.enqueue(ack.DetailRequest, time.Now()); err != nil {
			klog.V(2).Infof("Ignoring invalid detail command: %v", err)
		}
	}
}

func (s *nodeDetailState) acknowledge(ack *statusv1alpha1.NodeStatusAck) {
	if ack == nil || ack.DetailRequestID == "" || ack.Status != "ok" {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if reply := s.replies[ack.DetailRequestID]; reply != nil {
		reply.payload = nil
		reply.done = true
		reply.sending = false
		reply.collecting = false
		reply.ready = false
		reply.retryAt = time.Time{}
	}
}

type nodeDetailDelivery struct {
	id       string
	deadline time.Time
	payload  []byte
}

func (s *nodeDetailState) take(now time.Time) *nodeDetailDelivery {
	s.mu.Lock()
	s.expireLocked(now)

	var selected *nodeDetailReply

	for _, reply := range s.replies {
		if !reply.done && !reply.sending && reply.ready && !now.Before(reply.retryAt) {
			selected = reply
			selected.sending = true

			break
		}
	}
	s.mu.Unlock()

	if selected == nil {
		s.signalCollection()

		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.replies[selected.request.RequestID] != selected {
		return nil
	}

	if !selected.request.Deadline.After(time.Now()) {
		delete(s.replies, selected.request.RequestID)
		return nil
	}

	if selected.done {
		return nil
	}

	return &nodeDetailDelivery{id: selected.request.RequestID, deadline: selected.request.Deadline, payload: selected.payload}
}

func (s *nodeDetailState) finish(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if reply := s.replies[id]; reply != nil && !reply.done {
		reply.sending = false
		reply.retryAt = time.Now().Add(nodeDetailRetryInterval)
	}
}

func detailErrorPayload(nodeName, requestID, message string) []byte {
	payload, err := proto.Marshal(&statusproto.NodeStatusMessage{
		Type: statusv1alpha1.NodeStatusDetailsType, NodeName: nodeName, DetailRequestId: requestID,
		DetailError: strings.ToValidUTF8(message, "?"), SupportsDetails: true,
	})
	if err != nil {
		klog.Errorf("Failed to encode correlated detail failure: %v", err)
		return nil
	}

	return payload
}

func (s *nodeDetailState) failDelivery(nodeName string, delivery *nodeDetailDelivery, message string) []byte {
	payload := detailErrorPayload(nodeName, delivery.id, message)

	s.mu.Lock()
	if reply := s.replies[delivery.id]; reply != nil && !reply.done {
		reply.payload = payload
	}
	s.mu.Unlock()

	delivery.payload = payload

	return payload
}

func (s *nodeDetailState) wsPayload(nodeName string, delivery *nodeDetailDelivery) []byte {
	if len(delivery.payload) <= nodeDetailFrameLimit {
		return delivery.payload
	}

	return s.failDelivery(nodeName, delivery, "detail response exceeds 2 MiB WebSocket frame limit")
}

func collectDetailPayload(nodeName, requestID string, collect func() *NodeStatusResponse) (payload []byte) {
	defer func() {
		if failure := recover(); failure != nil {
			payload = detailErrorPayload(nodeName, requestID, fmt.Sprintf("detail collection failed: %v", failure))
		}
	}()

	full := collect()
	if full == nil {
		return detailErrorPayload(nodeName, requestID, "detail collection returned no snapshot")
	}

	if full.FetchError != "" {
		return detailErrorPayload(nodeName, requestID, full.FetchError)
	}

	message := &statusproto.NodeStatusMessage{
		Type: statusv1alpha1.NodeStatusDetailsType, NodeName: nodeName, DetailRequestId: requestID,
		Status: nodeStatusToProto(full), SupportsDetails: true,
	}

	payload, err := proto.Marshal(message)
	if err != nil {
		return detailErrorPayload(nodeName, requestID, fmt.Sprintf("detail encoding failed: %v", err))
	}

	return payload
}
