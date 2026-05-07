// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"net/http"
	"testing"
)

// TestHandleStatusPushRequest tests HandleStatusPushRequest.
func TestHandleStatusPushRequest(t *testing.T) {
	health := &healthState{statusCache: NewNodeStatusCache()}

	t.Run("invalid json", func(t *testing.T) {
		_, statusCode, err := handleStatusPushRequest(health, []byte("{bad"))
		if err == nil || statusCode != http.StatusBadRequest {
			t.Fatalf("expected bad request on invalid json, got status=%d err=%v", statusCode, err)
		}
	})

	t.Run("legacy full push requires node name", func(t *testing.T) {
		_, statusCode, err := handleStatusPushRequest(health, []byte(`{"wireGuardStatus":{"interface":"wg0"}}`))
		if err == nil || statusCode != http.StatusBadRequest {
			t.Fatalf("expected bad request for missing nodeInfo.name, got status=%d err=%v", statusCode, err)
		}
	})

	t.Run("full mode success", func(t *testing.T) {
		ack, statusCode, err := handleStatusPushRequest(health, []byte(`{
			"mode":"full",
			"nodeName":"node-a",
			"status":{"nodeInfo":{"siteName":"site-a"}}
		}`))
		if err != nil || statusCode != http.StatusOK {
			t.Fatalf("expected full mode success, got status=%d err=%v", statusCode, err)
		}

		if ack.Status != "ok" || ack.Revision == 0 {
			t.Fatalf("unexpected full ack: %#v", ack)
		}

		cached, ok := health.statusCache.Get("node-a")
		if !ok || cached.Status.NodeInfo.Name != "node-a" {
			t.Fatalf("expected full push to be stored with backfilled node name, got %#v", cached)
		}
	})

	t.Run("delta mode requires delta payload", func(t *testing.T) {
		_, statusCode, err := handleStatusPushRequest(health, []byte(`{"mode":"delta","nodeName":"node-a","baseRevision":1}`))
		if err == nil || statusCode != http.StatusBadRequest {
			t.Fatalf("expected bad request for missing delta payload, got status=%d err=%v", statusCode, err)
		}
	})

	t.Run("delta conflict returns resync", func(t *testing.T) {
		ack, statusCode, err := handleStatusPushRequest(health, []byte(`{
			"mode":"delta",
			"nodeName":"node-a",
			"baseRevision":999,
			"delta":{"wireGuardStatus":{"interface":"wg1"}}
		}`))
		if err != nil {
			t.Fatalf("expected conflict ack without handler error, got %v", err)
		}

		if statusCode != http.StatusTooManyRequests || ack.Status != "resync_required" {
			t.Fatalf("unexpected conflict response status=%d ack=%#v", statusCode, ack)
		}
	})

	t.Run("delta success", func(t *testing.T) {
		base, ok := health.statusCache.Get("node-a")
		if !ok {
			t.Fatalf("expected cached base state for node-a")
		}

		ack, statusCode, err := handleStatusPushRequest(health, []byte(`{
			"mode":"delta",
			"nodeName":"node-a",
			"baseRevision":1,
			"delta":{"wireGuardStatus":{"interface":"wg2"}}
		}`))
		if err != nil || statusCode != http.StatusOK {
			t.Fatalf("expected delta success, got status=%d err=%v", statusCode, err)
		}

		if ack.Status != "ok" || ack.Revision <= base.Revision {
			t.Fatalf("expected revision increment after delta, got ack=%#v base=%d", ack, base.Revision)
		}
	})

	t.Run("apiserver push source is preserved", func(t *testing.T) {
		_, statusCode, err := handleStatusPushRequestWithSource(health, []byte(`{
			"mode":"full",
			"nodeName":"node-apiserver",
			"status":{"nodeInfo":{"siteName":"site-a"}}
		}`), "apiserver-push")
		if err != nil || statusCode != http.StatusOK {
			t.Fatalf("expected full mode success, got status=%d err=%v", statusCode, err)
		}

		cached, ok := health.statusCache.Get("node-apiserver")
		if !ok {
			t.Fatalf("expected cached status for node-apiserver")
		}

		if cached.Source != "apiserver-push" {
			t.Fatalf("expected source apiserver-push, got %q", cached.Source)
		}
	})
}

// TestHandleNodeStatusWSMessage tests HandleNodeStatusWSMessage.
func TestHandleNodeStatusWSMessage(t *testing.T) {
	health := &healthState{statusCache: NewNodeStatusCache()}

	msgType, ack := handleNodeStatusWSMessage(health, []byte("bad-json"))
	if msgType != "node_status_resync" || ack.Status != "resync_required" {
		t.Fatalf("expected invalid json resync ack, got type=%q ack=%#v", msgType, ack)
	}

	msgType, ack = handleNodeStatusWSMessage(health, []byte(`{"type":"node_status_full"}`))
	if msgType != "node_status_resync" || ack.Reason != "nodeName is required" {
		t.Fatalf("expected missing nodeName resync, got type=%q ack=%#v", msgType, ack)
	}

	msgType, ack = handleNodeStatusWSMessage(health, []byte(`{"type":"node_status_full","nodeName":"node-a"}`))
	if msgType != "node_status_resync" || ack.Reason != "full message missing status" {
		t.Fatalf("expected missing full status resync, got type=%q ack=%#v", msgType, ack)
	}

	msgType, ack = handleNodeStatusWSMessage(health, []byte(`{
		"type":"node_status_full",
		"nodeName":"node-a",
		"status":{"nodeInfo":{"siteName":"site-a"}}
	}`))
	if msgType != "node_status_ack" || ack.Status != "ok" || ack.Revision == 0 {
		t.Fatalf("expected full ws ack success, got type=%q ack=%#v", msgType, ack)
	}

	msgType, ack = handleNodeStatusWSMessage(health, []byte(`{"type":"node_status_delta","nodeName":"node-a","baseRevision":1}`))
	if msgType != "node_status_resync" || ack.Reason != "delta message missing delta" {
		t.Fatalf("expected missing delta resync, got type=%q ack=%#v", msgType, ack)
	}

	msgType, ack = handleNodeStatusWSMessage(health, []byte(`{
		"type":"node_status_delta",
		"nodeName":"node-a",
		"baseRevision":999,
		"delta":{"wireGuardStatus":{"interface":"wg9"}}
	}`))
	if msgType != "node_status_resync" || ack.Reason != "base revision mismatch" {
		t.Fatalf("expected conflict resync, got type=%q ack=%#v", msgType, ack)
	}

	msgType, ack = handleNodeStatusWSMessage(health, []byte(`{
		"type":"node_status_delta",
		"nodeName":"node-a",
		"baseRevision":1,
		"delta":{"wireGuardStatus":{"interface":"wg10"}}
	}`))
	if msgType != "node_status_ack" || ack.Status != "ok" || ack.Revision < 2 {
		t.Fatalf("expected delta ws ack success, got type=%q ack=%#v", msgType, ack)
	}

	msgType, ack = handleNodeStatusWSMessage(health, []byte(`{"type":"node_status_unknown","nodeName":"node-a"}`))
	if msgType != "node_status_resync" || ack.Reason != "unsupported message type" {
		t.Fatalf("expected unsupported-type resync, got type=%q ack=%#v", msgType, ack)
	}

	msgType, ack = handleNodeStatusWSMessageWithSource(health, []byte(`{
		"type":"node_status_full",
		"nodeName":"node-b",
		"status":{"nodeInfo":{"siteName":"site-b"}}
	}`), "apiserver-ws")
	if msgType != "node_status_ack" || ack.Status != "ok" || ack.Revision == 0 {
		t.Fatalf("expected full ws ack success, got type=%q ack=%#v", msgType, ack)
	}

	cached, ok := health.statusCache.Get("node-b")
	if !ok {
		t.Fatalf("expected cached status for node-b")
	}

	if cached.Source != "apiserver-ws" {
		t.Fatalf("expected source apiserver-ws, got %q", cached.Source)
	}
}

// TestExtractNodeNameFromWSMessage tests ExtractNodeNameFromWSMessage.
func TestExtractNodeNameFromWSMessage(t *testing.T) {
	t.Run("uses top level nodeName", func(t *testing.T) {
		got := extractNodeNameFromWSMessage([]byte(`{"type":"node_status_delta","nodeName":"node-a"}`))
		if got != "node-a" {
			t.Fatalf("expected node-a, got %q", got)
		}
	})

	t.Run("falls back to status nodeInfo name", func(t *testing.T) {
		got := extractNodeNameFromWSMessage([]byte(`{"type":"node_status_full","status":{"nodeInfo":{"name":"node-b"}}}`))
		if got != "node-b" {
			t.Fatalf("expected node-b, got %q", got)
		}
	})

	t.Run("returns empty on invalid payload", func(t *testing.T) {
		got := extractNodeNameFromWSMessage([]byte("{bad"))
		if got != "" {
			t.Fatalf("expected empty node name, got %q", got)
		}
	})
}
