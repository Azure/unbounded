// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

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
		err = manager.CompleteFailure(nodeName, requestID, failure)
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
