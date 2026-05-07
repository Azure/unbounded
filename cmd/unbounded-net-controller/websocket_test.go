// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"encoding/json"
	"testing"
)

// TestComputeNodeDelta tests ComputeNodeDelta.
func TestComputeNodeDelta(t *testing.T) {
	t.Run("changed field", func(t *testing.T) {
		prev := []byte(`{"nodeInfo":{"name":"node-a"},"wireguard":{"peerCount":1},"routingTable":{"ipv4Routes":[]}}`)
		curr := []byte(`{"nodeInfo":{"name":"node-a"},"wireguard":{"peerCount":2},"routingTable":{"ipv4Routes":[]}}`)

		delta := computeNodeDelta(prev, curr)

		var got map[string]json.RawMessage
		if err := json.Unmarshal(delta, &got); err != nil {
			t.Fatalf("unmarshal delta: %v", err)
		}

		if _, ok := got["nodeInfo"]; !ok {
			t.Fatalf("expected nodeInfo in delta")
		}

		if _, ok := got["wireguard"]; !ok {
			t.Fatalf("expected changed wireguard field in delta")
		}

		if _, ok := got["routingTable"]; ok {
			t.Fatalf("did not expect unchanged routingTable in delta")
		}
	})

	t.Run("removed field", func(t *testing.T) {
		prev := []byte(`{"nodeInfo":{"name":"node-a"},"fetchError":"boom"}`)
		curr := []byte(`{"nodeInfo":{"name":"node-a"}}`)

		delta := computeNodeDelta(prev, curr)

		var got map[string]interface{}
		if err := json.Unmarshal(delta, &got); err != nil {
			t.Fatalf("unmarshal delta: %v", err)
		}

		if _, ok := got["nodeInfo"]; !ok {
			t.Fatalf("expected nodeInfo in delta")
		}

		if v, ok := got["fetchError"]; !ok || v != nil {
			t.Fatalf("expected removed field as null, got %#v", got["fetchError"])
		}
	})

	t.Run("invalid json falls back to current", func(t *testing.T) {
		curr := []byte(`{"nodeInfo":{"name":"node-a"}}`)

		delta := computeNodeDelta([]byte(`not-json`), curr)
		if string(delta) != string(curr) {
			t.Fatalf("expected fallback to current payload, got %s", string(delta))
		}
	})
}
