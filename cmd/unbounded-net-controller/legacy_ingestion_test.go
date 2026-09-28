// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"net/http"
	"testing"

	"google.golang.org/protobuf/proto"
	"k8s.io/apimachinery/pkg/types"

	statusproto "github.com/Azure/unbounded/internal/net/status/proto"
)

func TestLegacyIngestionPropagatesStorageFailure(t *testing.T) {
	message := &statusproto.NodeStatusMessage{
		Type: "node_status_full", NodeName: "node",
		Status: &statusproto.NodeStatusFull{
			NodeInfo: &statusproto.NodeInfo{Name: "node"},
			Peers:    []*statusproto.PeerStatus{{Name: "peer"}},
		},
	}

	binary, err := proto.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}

	decoded, err := decodeProtoWSMessage(binary)
	if err != nil {
		t.Fatal(err)
	}

	for _, failure := range []string{"none", "identity", "closed"} {
		t.Run(failure, func(t *testing.T) {
			for _, transport := range []string{"raw-http", "json-http", "protobuf-http", "json-ws", "protobuf-ws"} {
				t.Run(transport, func(t *testing.T) {
					manager := testDetailRequests(t, nodeDetailRequestHooks{
						Resolve: func(string) (types.UID, error) {
							if failure == "identity" {
								return "", errors.New("node identity unavailable")
							}

							return "uid", nil
						},
					})
					cache := NewNodeStatusCache()
					cache.BindDetails(manager)
					health := &healthState{statusCache: cache}

					if failure == "closed" {
						manager.Close()
					}

					var (
						ack     NodeStatusPushAck
						code    int
						ackType string
						pushErr error
					)

					switch transport {
					case "raw-http":
						ack, code, pushErr = handleStatusPushRequest(health, []byte(`{"nodeInfo":{"name":"node"},"peers":[{"name":"peer"}]}`))
					case "json-http":
						ack, code, pushErr = handleStatusPushRequest(health, []byte(`{"mode":"full","nodeName":"node","status":{"nodeInfo":{"name":"node"},"peers":[{"name":"peer"}]}}`))
					case "protobuf-http":
						ack, code, pushErr = handleProtoPushRequest(health, binary, "push")
					case "json-ws":
						ackType, ack = handleNodeStatusWSMessage(health, []byte(`{"type":"node_status_full","nodeName":"node","status":{"nodeInfo":{"name":"node"},"peers":[{"name":"peer"}]}}`))
					case "protobuf-ws":
						ackType, ack = handleProtoWSMessage(health, decoded, "ws")
					}

					if failure != "none" {
						if code != 0 && (code != http.StatusServiceUnavailable || pushErr == nil) {
							t.Fatalf("HTTP storage failure was hidden: code=%d ack=%+v err=%v", code, ack, pushErr)
						}

						if code == 0 && (ackType != "node_status_resync" || ack.Status != "resync_required" || ack.Reason == "") {
							t.Fatalf("WebSocket storage failure was hidden: type=%s ack=%+v", ackType, ack)
						}

						if ack.Revision != 0 || cache.Len() != 0 {
							t.Fatal("failed storage advanced publication state")
						}

						assertNodeDetailEntries(t, manager.cache, 0)

						return
					}

					if pushErr != nil || ack.Status != "ok" || ack.Revision != 1 ||
						(code != 0 && code != http.StatusOK) || (code == 0 && ackType != "node_status_ack") {
						t.Fatalf("successful storage was rejected: code=%d type=%s ack=%+v err=%v", code, ackType, ack, pushErr)
					}

					entry, ok := cache.Get("node")
					if !ok || entry.Overview == nil || entry.Overview.PeerCount != 1 || len(entry.Status.Peers) != 0 {
						t.Fatal("routine cache retained full peers or lost overview counts")
					}

					snapshot, ok := manager.cache.Get("node")
					if !ok || len(snapshot.Status.Peers) != 1 {
						t.Fatal("accepted publication lost expiring details")
					}
				})
			}
		})
	}
}
