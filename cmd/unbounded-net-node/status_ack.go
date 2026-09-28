// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"sync/atomic"

	"google.golang.org/protobuf/proto"

	netstatus "github.com/Azure/unbounded/internal/net/status"
	statusproto "github.com/Azure/unbounded/internal/net/status/proto"
	statusv1alpha1 "github.com/Azure/unbounded/internal/net/status/v1alpha1"
)

// statusAckState is created fresh for every connection. One outstanding message
// keeps the sender's snapshot and the controller's acknowledged revision aligned.
type statusAckState struct {
	revision atomic.Uint64
	resync   atomic.Bool
	pending  atomic.Bool
	compact  atomic.Bool
	summary  atomic.Bool
}

func decodeNodeStatusAck(data []byte) (*statusv1alpha1.NodeStatusAck, error) {
	var ack statusproto.NodeStatusAck
	if err := proto.Unmarshal(data, &ack); err == nil && ack.Status != "" {
		return netstatus.NodeStatusAckFromProto(&ack), nil
	}

	var jsonAck statusv1alpha1.NodeStatusAck
	if err := json.Unmarshal(data, &jsonAck); err == nil && jsonAck.Status != "" {
		return &jsonAck, nil
	}

	var envelope struct {
		Type string                       `json:"type"`
		Data statusv1alpha1.NodeStatusAck `json:"data"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil, err
	}

	switch envelope.Type {
	case "node_status_ack":
		if envelope.Data.Status == "" {
			envelope.Data.Status = "ok"
		}
	case "node_status_resync":
		envelope.Data.Status = "resync_required"
	default:
		return nil, fmt.Errorf("unrecognized status acknowledgment")
	}

	return &envelope.Data, nil
}

func (s *statusAckState) accept(data []byte) bool {
	ack, err := decodeNodeStatusAck(data)
	return err == nil && s.acceptAck(ack)
}

func (s *statusAckState) acceptAck(ack *statusv1alpha1.NodeStatusAck) bool {
	if !ack.IsPublicationAck() {
		return false
	}

	s.summary.Store(ack.SummarySupported)

	switch ack.Status {
	case "ok":
		s.compact.Store(ack.PeerMeasurements && ack.Revision > 0)
	case "resync_required":
		s.compact.Store(false)
		s.resync.Store(true)
	default:
		return false
	}

	if ack.Revision > 0 {
		s.revision.Store(ack.Revision)
	}

	s.pending.Store(false)

	return true
}
