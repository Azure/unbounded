// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package status

import (
	"encoding/json"
	"testing"

	"google.golang.org/protobuf/proto"

	statusproto "github.com/Azure/unbounded/internal/net/status/proto"
	"github.com/Azure/unbounded/internal/net/status/v1alpha1"
)

func TestCorrelatedDetailError(t *testing.T) {
	message := &statusproto.NodeStatusMessage{
		Type: v1alpha1.NodeStatusDetailsType, NodeName: "node",
		DetailRequestId: "request", DetailError: "detail response exceeds transport limit",
	}

	data, err := proto.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}

	var decoded statusproto.NodeStatusMessage
	if err := proto.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}

	if !proto.Equal(message, &decoded) || decoded.Status != nil || decoded.BaseRevision != 0 {
		t.Fatalf("failure lost correlation or became a publication: %v", &decoded)
	}

	jsonMessage := v1alpha1.NodeStatusMessage{
		Type: message.Type, NodeName: message.NodeName, DetailRequestID: message.DetailRequestId, DetailError: message.DetailError,
	}

	data, err = json.Marshal(jsonMessage)
	if err != nil {
		t.Fatal(err)
	}

	var jsonDecoded v1alpha1.NodeStatusMessage
	if err := json.Unmarshal(data, &jsonDecoded); err != nil {
		t.Fatal(err)
	}

	if jsonDecoded.DetailError != message.DetailError || jsonDecoded.DetailRequestID != message.DetailRequestId || jsonDecoded.Status != nil {
		t.Fatalf("JSON failure lost facts: %+v", jsonDecoded)
	}
}
