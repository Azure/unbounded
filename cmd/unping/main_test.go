// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	pb "github.com/Azure/unbounded/internal/net/healthcheck/proto"
)

// probeHarness wires a client socket to a local responder that answers every
// health check request, so runProbes can be exercised without a remote node.
type probeHarness struct {
	ctx     context.Context
	cancel  func()
	replies chan replyResult
	send    func(seq uint64)
	seen    func() []uint64
}

func newProbeHarness(t *testing.T) *probeHarness {
	t.Helper()

	responder, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("failed to bind responder socket: %v", err)
	}

	t.Cleanup(func() { _ = responder.Close() })

	client, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("failed to bind client socket: %v", err)
	}

	t.Cleanup(func() { _ = client.Close() })

	var (
		mu   sync.Mutex
		seen []uint64
	)

	go func() {
		buf := make([]byte, 1500)

		for {
			n, remote, err := responder.ReadFromUDP(buf)
			if err != nil {
				return
			}

			pkt := &pb.HealthCheckPacket{}
			if err := proto.Unmarshal(buf[:n], pkt); err != nil {
				continue
			}

			mu.Lock()
			seen = append(seen, pkt.SequenceNumber)
			mu.Unlock()

			reply, err := proto.Marshal(&pb.HealthCheckPacket{
				SourceHostname:      pkt.DestinationHostname,
				DestinationHostname: pkt.SourceHostname,
				SequenceNumber:      pkt.SequenceNumber,
				TimestampNs:         pkt.TimestampNs,
				Type:                pb.PacketType_REPLY,
			})
			if err != nil {
				continue
			}

			if _, err := responder.WriteToUDP(reply, remote); err != nil {
				return
			}
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	replies := make(chan replyResult, 16)
	go receiveReplies(ctx, client, replies)

	responderAddr := responder.LocalAddr().(*net.UDPAddr)

	return &probeHarness{
		ctx:     ctx,
		cancel:  cancel,
		replies: replies,
		send: func(seq uint64) {
			sendProbe(client, responderAddr, "src", "dst", seq)
		},
		seen: func() []uint64 {
			mu.Lock()
			defer mu.Unlock()

			return append([]uint64(nil), seen...)
		},
	}
}

func TestRunProbesSendsExactlyCountProbes(t *testing.T) {
	tests := []struct {
		name     string
		count    int
		interval time.Duration
	}{
		{name: "single probe", count: 1, interval: 500 * time.Millisecond},
		{name: "multiple probes", count: 3, interval: 10 * time.Millisecond},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newProbeHarness(t)

			start := time.Now()
			stats := runProbes(h.ctx, tt.count, tt.interval, 2*time.Second, h.send, h.replies, net.IPv4(127, 0, 0, 1), io.Discard)
			elapsed := time.Since(start)

			// The bug sent one probe past the count, so leave room for a stray
			// datagram to arrive before checking what the responder saw.
			time.Sleep(tt.interval + 100*time.Millisecond)

			seen := h.seen()
			if len(seen) != tt.count {
				t.Fatalf("responder received %d probes, want %d: %v", len(seen), tt.count, seen)
			}

			for i, seq := range seen {
				if want := uint64(i + 1); seq != want {
					t.Errorf("probe %d carried seq=%d, want %d", i, seq, want)
				}
			}

			if stats.sent != tt.count {
				t.Errorf("stats.sent = %d, want %d", stats.sent, tt.count)
			}

			if stats.received != tt.count {
				t.Errorf("stats.received = %d, want %d", stats.received, tt.count)
			}

			// The last probe does not wait for another interval; with more than
			// one probe the interval only separates the probes that precede it.
			if tt.count == 1 && elapsed >= tt.interval {
				t.Errorf("runProbes took %v for %d probe, want less than one interval (%v)", elapsed, tt.count, tt.interval)
			}
		})
	}
}

func TestRunProbesZeroCountProbesUntilCanceled(t *testing.T) {
	h := newProbeHarness(t)

	done := make(chan probeStats, 1)
	go func() {
		done <- runProbes(h.ctx, 0, 5*time.Millisecond, time.Second, h.send, h.replies, net.IPv4(127, 0, 0, 1), io.Discard)
	}()

	// Wait for a few probes to prove that zero means "until canceled" rather
	// than "send nothing".
	deadline := time.Now().Add(2 * time.Second)
	for len(h.seen()) < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("responder received %d probes, want at least 3", len(h.seen()))
		}

		time.Sleep(2 * time.Millisecond)
	}

	// Canceling must stop the loop promptly.
	h.cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runProbes did not stop after the context was canceled")
	}
}
