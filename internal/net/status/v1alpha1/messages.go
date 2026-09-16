// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"encoding/json"
	"time"
)

const (
	NodeStatusSummaryType = "node_status_summary"
	NodeStatusDetailsType = "node_status_details"
	DetailRequestStatus   = "detail_request"
)

// DetailRequest is a node-bound diagnostic command, independent of revisions.
type DetailRequest struct {
	RequestID string    `json:"requestId"`
	Deadline  time.Time `json:"deadline"`
}

// NodeStatusMessage is the JSON equivalent of the protobuf status envelope.
// Status holds legacy full publications or one-shot node_status_details replies.
type NodeStatusMessage struct {
	Type            string                     `json:"type"`
	NodeName        string                     `json:"nodeName,omitempty"`
	BaseRevision    uint64                     `json:"baseRevision,omitempty"`
	Status          *NodeStatusResponse        `json:"status,omitempty"`
	Delta           map[string]json.RawMessage `json:"delta,omitempty"`
	Summary         *NodeStatusOverview        `json:"summary,omitempty"`
	DetailRequestID string                     `json:"detailRequestId,omitempty"`
	SupportsDetails bool                       `json:"supportsDetails,omitempty"`
	DetailError     string                     `json:"detailError,omitempty"`
}

// NodeStatusAck is shared by HTTP responses and WebSocket ACK/command data.
// An unsolicited command uses DetailRequestStatus and leaves Revision unset.
// An HTTP publication ACK may also carry a pending DetailRequest.
type NodeStatusAck struct {
	Status           string         `json:"status"`
	Revision         uint64         `json:"revision,omitempty"`
	Reason           string         `json:"reason,omitempty"`
	PeerMeasurements bool           `json:"peerMeasurements,omitempty"`
	DetailRequest    *DetailRequest `json:"detailRequest,omitempty"`
	SummarySupported bool           `json:"summarySupported,omitempty"`
	DetailRequestID  string         `json:"detailRequestId,omitempty"`
}

// IsPublicationAck excludes standalone commands and one-shot detail ACKs.
// Only publication ACKs may clear a routine publisher's pending ACK state.
func (a *NodeStatusAck) IsPublicationAck() bool {
	return a != nil && a.DetailRequestID == "" &&
		(a.Status == "ok" || a.Status == "resync_required")
}
