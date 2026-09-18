// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package status

import (
	"fmt"
	"time"

	statusproto "github.com/Azure/unbounded/internal/net/status/proto"
	statusv1alpha1 "github.com/Azure/unbounded/internal/net/status/v1alpha1"
)

// ValidateDetailRequest rejects missing identities and expired/unset deadlines.
func ValidateDetailRequest(request *statusv1alpha1.DetailRequest, now time.Time) error {
	if request == nil || request.RequestID == "" {
		return fmt.Errorf("detail request ID is required")
	}

	if request.Deadline.IsZero() || !request.Deadline.After(now) {
		return fmt.Errorf("detail request deadline must be in the future")
	}

	return nil
}

// DetailRequestToProto preserves nil requests and uses zero for unset deadlines.
func DetailRequestToProto(request *statusv1alpha1.DetailRequest) *statusproto.DetailRequest {
	if request == nil {
		return nil
	}

	result := &statusproto.DetailRequest{RequestId: request.RequestID}
	if !request.Deadline.IsZero() {
		result.DeadlineUnixNs = request.Deadline.UnixNano()
	}

	return result
}

// DetailRequestFromProto preserves nil requests and unset deadlines.
func DetailRequestFromProto(request *statusproto.DetailRequest) *statusv1alpha1.DetailRequest {
	if request == nil {
		return nil
	}

	result := &statusv1alpha1.DetailRequest{RequestID: request.RequestId}
	if request.DeadlineUnixNs != 0 {
		result.Deadline = time.Unix(0, request.DeadlineUnixNs).UTC()
	}

	return result
}

// NodeStatusAckToProto converts shared ACKs without conflating request IDs and revisions.
func NodeStatusAckToProto(ack *statusv1alpha1.NodeStatusAck) *statusproto.NodeStatusAck {
	if ack == nil {
		return nil
	}

	return &statusproto.NodeStatusAck{
		Status: ack.Status, Revision: ack.Revision, Reason: ack.Reason,
		PeerMeasurements: ack.PeerMeasurements,
		DetailRequest:    DetailRequestToProto(ack.DetailRequest),
		SummarySupported: ack.SummarySupported, DetailRequestId: ack.DetailRequestID,
	}
}

// NodeStatusAckFromProto converts shared ACKs for either response transport.
func NodeStatusAckFromProto(ack *statusproto.NodeStatusAck) *statusv1alpha1.NodeStatusAck {
	if ack == nil {
		return nil
	}

	return &statusv1alpha1.NodeStatusAck{
		Status: ack.Status, Revision: ack.Revision, Reason: ack.Reason,
		PeerMeasurements: ack.PeerMeasurements,
		DetailRequest:    DetailRequestFromProto(ack.DetailRequest),
		SummarySupported: ack.SummarySupported, DetailRequestID: ack.DetailRequestId,
	}
}
