// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"sync/atomic"

	"google.golang.org/protobuf/proto"

	statusproto "github.com/Azure/unbounded/internal/net/status/proto"
)

// statusAckState is created fresh for every connection. One outstanding message
// keeps the sender's snapshot and the controller's acknowledged revision aligned.
type statusAckState struct {
	revision atomic.Uint64
	resync   atomic.Bool
	pending  atomic.Bool
	compact  atomic.Bool
}

func (s *statusAckState) accept(data []byte) bool {
	var ack statusproto.NodeStatusAck
	if err := proto.Unmarshal(data, &ack); err != nil {
		var envelope struct {
			Type string            `json:"type"`
			Data nodeStatusPushAck `json:"data"`
		}
		if err := json.Unmarshal(data, &envelope); err != nil {
			return false
		}

		switch envelope.Type {
		case "node_status_ack":
			ack.Status = "ok"
		case "node_status_resync":
			ack.Status = "resync_required"
		default:
			return false
		}

		ack.Revision = envelope.Data.Revision
	}

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
