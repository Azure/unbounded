// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package status

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	statusproto "github.com/Azure/unbounded/internal/net/status/proto"
	statusv1alpha1 "github.com/Azure/unbounded/internal/net/status/v1alpha1"
)

func TestDetailRequestValidation(t *testing.T) {
	now := time.Unix(100, 0)
	for _, tc := range []struct {
		name    string
		request *statusv1alpha1.DetailRequest
		valid   bool
	}{
		{"nil", nil, false},
		{"missing ID", &statusv1alpha1.DetailRequest{Deadline: now.Add(time.Second)}, false},
		{"missing deadline", &statusv1alpha1.DetailRequest{RequestID: "id"}, false},
		{"expired", &statusv1alpha1.DetailRequest{RequestID: "id", Deadline: now.Add(-time.Nanosecond)}, false},
		{"at deadline", &statusv1alpha1.DetailRequest{RequestID: "id", Deadline: now}, false},
		{"live", &statusv1alpha1.DetailRequest{RequestID: "id", Deadline: now.Add(time.Nanosecond)}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateDetailRequest(tc.request, now); (err == nil) != tc.valid {
				t.Fatalf("ValidateDetailRequest() = %v, valid = %v", err, tc.valid)
			}
		})
	}
}

func TestDetailACKRoundTrip(t *testing.T) {
	request := &statusv1alpha1.DetailRequest{RequestID: "request", Deadline: time.Unix(100, 123).UTC()}
	for _, tc := range []struct {
		name        string
		ack         *statusv1alpha1.NodeStatusAck
		publication bool
	}{
		{"nil", nil, false},
		{"legacy", &statusv1alpha1.NodeStatusAck{Status: "ok", Revision: 5}, true},
		{"resync", &statusv1alpha1.NodeStatusAck{Status: "resync_required", Reason: "base expired"}, true},
		{"command", &statusv1alpha1.NodeStatusAck{Status: statusv1alpha1.DetailRequestStatus, DetailRequest: request}, false},
		{"details", &statusv1alpha1.NodeStatusAck{Status: "ok", DetailRequestID: "request"}, false},
		{"unknown", &statusv1alpha1.NodeStatusAck{Status: "unknown"}, false},
		{"piggyback", &statusv1alpha1.NodeStatusAck{
			Status: "ok", Revision: 9, DetailRequest: request, SummarySupported: true, PeerMeasurements: true,
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pb := NodeStatusAckToProto(tc.ack)
			if pb != nil {
				data, err := proto.Marshal(pb)
				if err != nil {
					t.Fatal(err)
				}

				pb = &statusproto.NodeStatusAck{}
				if err := proto.Unmarshal(data, pb); err != nil {
					t.Fatal(err)
				}
			}

			got := NodeStatusAckFromProto(pb)
			if !reflect.DeepEqual(got, tc.ack) || got.IsPublicationAck() != tc.publication {
				t.Fatalf("ACK changed or misclassified: %+v", got)
			}

			data, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}

			var jsonAck *statusv1alpha1.NodeStatusAck
			if err := json.Unmarshal(data, &jsonAck); err != nil {
				t.Fatal(err)
			}

			if !reflect.DeepEqual(jsonAck, tc.ack) || jsonAck.IsPublicationAck() != tc.publication {
				t.Fatalf("JSON ACK changed or misclassified: %s", data)
			}
		})
	}

	if got := DetailRequestFromProto(DetailRequestToProto(&statusv1alpha1.DetailRequest{RequestID: "unset"})); !got.Deadline.IsZero() {
		t.Fatalf("unset deadline changed: %v", got.Deadline)
	}
}

func TestDetailWireFields(t *testing.T) {
	for _, tc := range []struct {
		message proto.Message
		fields  map[protoreflect.Name]protoreflect.FieldNumber
	}{
		{&statusproto.NodeStatusMessage{}, map[protoreflect.Name]protoreflect.FieldNumber{
			"status": 4, "summary": 6, "detail_request_id": 7, "supports_details": 8,
		}},
		{&statusproto.NodeStatusAck{}, map[protoreflect.Name]protoreflect.FieldNumber{
			"status": 1, "revision": 2, "reason": 3, "peer_measurements": 4,
			"detail_request": 5, "summary_supported": 6, "detail_request_id": 7,
		}},
		{&statusproto.DetailRequest{}, map[protoreflect.Name]protoreflect.FieldNumber{
			"request_id": 1, "deadline_unix_ns": 2,
		}},
	} {
		fields := tc.message.ProtoReflect().Descriptor().Fields()
		for name, number := range tc.fields {
			if field := fields.ByName(name); field == nil || field.Number() != number {
				t.Errorf("%T field %q no longer has number %d", tc.message, name, number)
			}
		}
	}

	legacy := &statusv1alpha1.NodeStatusAck{Status: "ok", Revision: 1}

	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}

	if string(data) != `{"status":"ok","revision":1}` {
		t.Fatalf("legacy JSON shape changed: %s", data)
	}

	message := &statusv1alpha1.NodeStatusMessage{
		Type: statusv1alpha1.NodeStatusDetailsType, NodeName: "node", DetailRequestID: "request",
		SupportsDetails: true, Status: &statusv1alpha1.NodeStatusResponse{},
	}

	data, err = json.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}

	var got statusv1alpha1.NodeStatusMessage
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(&got, message) || got.Summary != nil || got.BaseRevision != 0 {
		t.Fatalf("detail JSON round trip changed payload: %s", data)
	}
}
