// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"

	statusproto "github.com/Azure/unbounded/internal/net/status/proto"
	statusv1alpha1 "github.com/Azure/unbounded/internal/net/status/v1alpha1"
)

// These legacy JSON helpers are retained only for existing compatibility tests
// and baseline benchmarks. Production publishers exclusively use typedStatusDelta,
// whose tests enforce critical metadata changes and explicit field clearings.
func computeStatusDelta(prev, curr *NodeStatusResponse) (map[string]json.RawMessage, error) {
	if prev == nil {
		return nil, nil
	}

	prevRaw, err := json.Marshal(prev)
	if err != nil {
		return nil, err
	}

	currRaw, err := json.Marshal(curr)
	if err != nil {
		return nil, err
	}

	var prevMap map[string]json.RawMessage
	if err := json.Unmarshal(prevRaw, &prevMap); err != nil {
		return nil, err
	}

	var currMap map[string]json.RawMessage
	if err := json.Unmarshal(currRaw, &currMap); err != nil {
		return nil, err
	}

	delta := make(map[string]json.RawMessage)
	if nodeInfo, ok := currMap["nodeInfo"]; ok {
		delta["nodeInfo"] = nodeInfo
	}

	for key, value := range currMap {
		if key == "nodeInfo" {
			continue
		}

		prevValue, exists := prevMap[key]
		if !exists || !bytes.Equal(prevValue, value) {
			delta[key] = value
		}
	}

	if _, previouslyPresent := prevMap["nodeErrors"]; previouslyPresent {
		if _, currentlyPresent := currMap["nodeErrors"]; !currentlyPresent {
			delta["nodeErrors"] = json.RawMessage("[]")
		}
	}
	// Preserve the old omission behavior for baseline comparisons only.
	if len(delta) == 1 {
		return nil, nil
	}

	return delta, nil
}

func nodeStatusDeltaToProto(delta map[string]json.RawMessage) *statusproto.NodeStatusDelta {
	if len(delta) == 0 {
		return nil
	}

	pb := &statusproto.NodeStatusDelta{
		UpdatedFields: make([]string, 0, len(delta)),
	}
	for key, raw := range delta {
		pb.UpdatedFields = append(pb.UpdatedFields, key)

		switch key {
		case "nodeInfo":
			var ni NodeInfo
			if json.Unmarshal(raw, &ni) == nil {
				pb.NodeInfo = nodeInfoToProto(&ni)
			}
		case "peers":
			var peers []statusv1alpha1.PeerStatus
			if json.Unmarshal(raw, &peers) == nil {
				pb.Peers = peersToProto(peers)
			}
		case "routingTable":
			var rt RoutingTableInfo
			if json.Unmarshal(raw, &rt) == nil {
				pb.RoutingTable = routingTableToProto(&rt)
			}
		case "healthCheck":
			var hc HealthCheckStatus
			if json.Unmarshal(raw, &hc) == nil {
				pb.HealthCheck = healthCheckStatusToProto(&hc)
			}
		case "nodeErrors":
			var errs []NodeError
			if json.Unmarshal(raw, &errs) == nil {
				pb.NodeErrors = nodeErrorsToProto(errs)
			}
		case "bpfEntries":
			var entries []BpfEntry
			if json.Unmarshal(raw, &entries) == nil {
				pb.BpfEntries = bpfEntriesToProto(entries)
			}
		}
	}

	return pb
}
