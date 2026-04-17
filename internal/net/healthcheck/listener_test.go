// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package healthcheck

import (
	"context"
	"net"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	pb "github.com/Azure/unbounded-kube/internal/net/healthcheck/proto"
)

func TestListenerRepliestoRequest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	listener, err := NewListener("node-a", 0)
	if err != nil {
		t.Fatalf("NewListener: %v", err)
	}

	// Bind to a random port.
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("ListenPacket: %v", err)
	}

	listener.conn = conn
	listenerAddr := conn.LocalAddr()

	go func() {
		_ = listener.Start(ctx)
	}()

	defer listener.Stop()

	// Give the listener a moment to start reading.
	time.Sleep(50 * time.Millisecond)

	// Send a request from a client socket.
	client, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("client ListenPacket: %v", err)
	}

	defer func() { _ = client.Close() }()

	req := &pb.HealthCheckPacket{
		SourceHostname:      "node-b",
		DestinationHostname: "node-a",
		SequenceNumber:      42,
		TimestampNs:         time.Now().UnixNano(),
		Type:                pb.PacketType_REQUEST,
	}

	data, err := proto.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	if _, err := client.WriteTo(data, listenerAddr); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}

	// Read the reply.
	if err := client.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}

	buf := make([]byte, maxPacketSize)

	n, _, err := client.ReadFrom(buf)
	if err != nil {
		t.Fatalf("ReadFrom: %v", err)
	}

	reply := &pb.HealthCheckPacket{}
	if err := proto.Unmarshal(buf[:n], reply); err != nil {
		t.Fatalf("unmarshal reply: %v", err)
	}

	if reply.Type != pb.PacketType_REPLY {
		t.Errorf("expected REPLY, got %v", reply.Type)
	}

	if reply.SourceHostname != "node-a" {
		t.Errorf("expected source node-a, got %q", reply.SourceHostname)
	}

	if reply.DestinationHostname != "node-b" {
		t.Errorf("expected destination node-b, got %q", reply.DestinationHostname)
	}

	if reply.SequenceNumber != 42 {
		t.Errorf("expected seq 42, got %d", reply.SequenceNumber)
	}
}

func TestListenerIgnoresMismatchedHostname(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	listener, err := NewListener("node-a", 0)
	if err != nil {
		t.Fatalf("NewListener: %v", err)
	}

	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("ListenPacket: %v", err)
	}

	listener.conn = conn
	listenerAddr := conn.LocalAddr()

	go func() {
		_ = listener.Start(ctx)
	}()

	defer listener.Stop()

	time.Sleep(50 * time.Millisecond)

	client, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("client ListenPacket: %v", err)
	}

	defer func() { _ = client.Close() }()

	// Send request destined for a different hostname.
	req := &pb.HealthCheckPacket{
		SourceHostname:      "node-b",
		DestinationHostname: "node-c",
		SequenceNumber:      1,
		TimestampNs:         time.Now().UnixNano(),
		Type:                pb.PacketType_REQUEST,
	}

	data, _ := proto.Marshal(req)
	if _, err := client.WriteTo(data, listenerAddr); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}

	// Should not get a reply.
	if err := client.SetReadDeadline(time.Now().Add(200 * time.Millisecond)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}

	buf := make([]byte, maxPacketSize)

	_, _, err = client.ReadFrom(buf)
	if err == nil {
		t.Error("expected timeout, but got a reply")
	}
}

func TestNewListenerInvalidPort(t *testing.T) {
	_, err := NewListener("test", -1)
	if err == nil {
		t.Error("expected error for negative port")
	}

	_, err = NewListener("test", 70000)
	if err == nil {
		t.Error("expected error for port > 65535")
	}
	// Port 0 should be valid (OS-assigned).
	l, err := NewListener("test", 0)
	if err != nil {
		t.Errorf("port 0 should be valid: %v", err)
	}

	if l == nil {
		t.Error("expected non-nil listener for port 0")
	}
}
