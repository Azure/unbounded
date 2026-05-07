// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package healthcheck

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	pb "github.com/Azure/unbounded/internal/net/healthcheck/proto"
)

func TestSessionGoesUpOnReply(t *testing.T) {
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("ListenPacket: %v", err)
	}

	defer func() { _ = conn.Close() }()

	stateChanges := make(chan SessionState, 10)

	detectMult := 3
	s := newSession(sessionConfig{
		peerHostname:  "peer-a",
		overlayIP:     net.ParseIP("127.0.0.1"),
		port:          conn.LocalAddr().(*net.UDPAddr).Port,
		localHostname: "local",
		settings: HealthCheckSettings{
			TransmitInterval: 50 * time.Millisecond,
			ReceiveInterval:  50 * time.Millisecond,
			DetectMultiplier: detectMult,
		},
		onChange: func(peer string, newState, oldState SessionState) {
			stateChanges <- newState
		},
		conn: conn,
	})

	if s.state != StateDown {
		t.Fatalf("expected initial state Down, got %s", s.state)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s.start(ctx)
	defer s.stop()

	// Send fewer than DetectMultiplier replies -- session must stay Down.
	for i := 0; i < detectMult-1; i++ {
		pkt := &pb.HealthCheckPacket{
			SourceHostname: "peer-a",
			TimestampNs:    time.Now().UnixNano(),
			Type:           pb.PacketType_REPLY,
		}
		s.receiveReply(pkt)
	}

	select {
	case state := <-stateChanges:
		t.Fatalf("unexpected state change to %s before reaching DetectMultiplier", state)
	case <-time.After(100 * time.Millisecond):
		// Good -- no state change yet.
	}

	status := s.status()
	if status.State != StateDown {
		t.Errorf("expected status Down after %d replies, got %s", detectMult-1, status.State)
	}

	// Send the final reply to reach DetectMultiplier -- session should go Up.
	pkt := &pb.HealthCheckPacket{
		SourceHostname: "peer-a",
		TimestampNs:    time.Now().UnixNano(),
		Type:           pb.PacketType_REPLY,
	}
	s.receiveReply(pkt)

	select {
	case state := <-stateChanges:
		if state != StateUp {
			t.Errorf("expected StateUp, got %s", state)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for state change")
	}

	status = s.status()
	if status.State != StateUp {
		t.Errorf("expected status Up, got %s", status.State)
	}

	if status.PacketsReceived != uint64(detectMult) {
		t.Errorf("expected %d packets received, got %d", detectMult, status.PacketsReceived)
	}

	if status.RequiredReplies != detectMult {
		t.Errorf("expected RequiredReplies=%d, got %d", detectMult, status.RequiredReplies)
	}
}

func TestSessionGoesDownOnTimeout(t *testing.T) {
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("ListenPacket: %v", err)
	}

	defer func() { _ = conn.Close() }()

	stateChanges := make(chan SessionState, 10)

	s := newSession(sessionConfig{
		peerHostname:  "peer-b",
		overlayIP:     net.ParseIP("127.0.0.1"),
		port:          conn.LocalAddr().(*net.UDPAddr).Port,
		localHostname: "local",
		settings: HealthCheckSettings{
			TransmitInterval: 50 * time.Millisecond,
			ReceiveInterval:  50 * time.Millisecond,
			DetectMultiplier: 2,
		},
		onChange: func(peer string, newState, oldState SessionState) {
			stateChanges <- newState
		},
		conn: conn,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start the session before bringing it up so callbackLoop is running.
	s.start(ctx)
	defer s.stop()

	// Bring it up by sending DetectMultiplier replies.
	for i := 0; i < 2; i++ {
		pkt := &pb.HealthCheckPacket{
			SourceHostname: "peer-b",
			TimestampNs:    time.Now().UnixNano(),
			Type:           pb.PacketType_REPLY,
		}
		s.receiveReply(pkt)
	}

	<-stateChanges // consume Up

	// Wait for the timeout: detectMultiplier(2) * max(50ms, 50ms) = 100ms
	// The detect loop checks at half that interval (50ms), so within ~200ms
	// we should see a Down transition.
	select {
	case state := <-stateChanges:
		if state != StateDown {
			t.Errorf("expected StateDown, got %s", state)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Down state")
	}
}

func TestSessionUpdateSettings(t *testing.T) {
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("ListenPacket: %v", err)
	}

	defer func() { _ = conn.Close() }()

	s := newSession(sessionConfig{
		peerHostname:  "peer-c",
		overlayIP:     net.ParseIP("127.0.0.1"),
		port:          1234,
		localHostname: "local",
		settings:      DefaultSettings(),
		conn:          conn,
	})

	newSettings := HealthCheckSettings{
		TransmitInterval: 500 * time.Millisecond,
		ReceiveInterval:  500 * time.Millisecond,
		DetectMultiplier: 5,
	}
	s.updateSettings(newSettings)

	s.mu.Lock()
	if s.settings.DetectMultiplier != 5 {
		t.Errorf("expected multiplier 5, got %d", s.settings.DetectMultiplier)
	}
	s.mu.Unlock()
}

func TestSessionSendsProbes(t *testing.T) {
	// Create a "remote" listener to receive probes.
	remote, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("ListenPacket: %v", err)
	}

	defer func() { _ = remote.Close() }()

	sender, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("ListenPacket: %v", err)
	}

	defer func() { _ = sender.Close() }()

	remotePort := remote.LocalAddr().(*net.UDPAddr).Port

	s := newSession(sessionConfig{
		peerHostname:  "peer-d",
		overlayIP:     net.ParseIP("127.0.0.1"),
		port:          remotePort,
		localHostname: "local",
		settings: HealthCheckSettings{
			TransmitInterval: 50 * time.Millisecond,
			ReceiveInterval:  50 * time.Millisecond,
			DetectMultiplier: 3,
		},
		conn: sender,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s.start(ctx)
	defer s.stop()

	// Read at least one probe from the remote side.
	if err := remote.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}

	buf := make([]byte, maxPacketSize)

	n, _, err := remote.ReadFrom(buf)
	if err != nil {
		t.Fatalf("ReadFrom: %v", err)
	}

	if n == 0 {
		t.Fatal("received empty packet")
	}

	status := s.status()
	if status.PacketsSent == 0 {
		t.Error("expected at least one packet sent")
	}
}

// makeReply creates a reply packet for use in tests.
func makeReply(peer string) *pb.HealthCheckPacket {
	return &pb.HealthCheckPacket{
		SourceHostname: peer,
		TimestampNs:    time.Now().UnixNano(),
		Type:           pb.PacketType_REPLY,
	}
}

// sendNReplies sends n reply packets to the session.
func sendNReplies(s *session, peer string, n int) {
	for i := 0; i < n; i++ {
		s.receiveReply(makeReply(peer))
	}
}

// waitForSessionState waits up to timeout for the expected state on the channel.
func waitForSessionState(t *testing.T, ch <-chan SessionState, expected SessionState, timeout time.Duration, label string) {
	t.Helper()

	select {
	case state := <-ch:
		if state != expected {
			t.Errorf("%s: expected %s, got %s", label, expected, state)
		}
	case <-time.After(timeout):
		t.Fatalf("%s: timed out waiting for %s", label, expected)
	}
}

func TestStaleCallbackSkipped(t *testing.T) {
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("ListenPacket: %v", err)
	}

	defer func() { _ = conn.Close() }()

	var (
		mu             sync.Mutex
		callbackStates []SessionState
	)

	s := newSession(sessionConfig{
		peerHostname:  "peer-stale",
		overlayIP:     net.ParseIP("127.0.0.1"),
		port:          conn.LocalAddr().(*net.UDPAddr).Port,
		localHostname: "local",
		settings: HealthCheckSettings{
			TransmitInterval: 10 * time.Millisecond,
			ReceiveInterval:  10 * time.Millisecond,
			DetectMultiplier: 2,
			MaxBackoff:       10 * time.Second,
		},
		onChange: func(peer string, newState, oldState SessionState) {
			mu.Lock()

			callbackStates = append(callbackStates, newState)
			mu.Unlock()
		},
		conn: conn,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s.start(ctx)
	defer s.stop()

	// Rapidly alternate: send replies -> Up, wait for timeout -> Down.
	for i := 0; i < 3; i++ {
		required := s.status().RequiredReplies
		sendNReplies(s, "peer-stale", required)
		// Wait for detect loop to notice the timeout and bring the session Down.
		// Timeout = 2 * 10ms = 20ms; detect loop minimum interval = 100ms.
		time.Sleep(300 * time.Millisecond)
	}

	// Allow all pending callbacks to drain.
	time.Sleep(200 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()

	if len(callbackStates) == 0 {
		t.Fatal("expected at least one callback to be delivered")
	}

	// No callback should fire with a state that doesn't match the session's
	// current state at invocation time. Because the callbackLoop filters stale
	// events, we should never see two consecutive callbacks with the same state.
	for i := 1; i < len(callbackStates); i++ {
		if callbackStates[i] == callbackStates[i-1] {
			t.Errorf("stale callback delivered: consecutive callbacks both %s at indices %d and %d",
				callbackStates[i], i-1, i)
		}
	}

	// Verify we saw both Up and Down transitions.
	seenUp, seenDown := false, false

	for _, s := range callbackStates {
		if s == StateUp {
			seenUp = true
		}

		if s == StateDown {
			seenDown = true
		}
	}

	if !seenUp {
		t.Error("expected at least one Up callback")
	}

	if !seenDown {
		t.Error("expected at least one Down callback")
	}
}

func TestConsecutiveRepliesRequired(t *testing.T) {
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("ListenPacket: %v", err)
	}

	defer func() { _ = conn.Close() }()

	stateChanges := make(chan SessionState, 10)

	s := newSession(sessionConfig{
		peerHostname:  "peer-consec",
		overlayIP:     net.ParseIP("127.0.0.1"),
		port:          conn.LocalAddr().(*net.UDPAddr).Port,
		localHostname: "local",
		settings: HealthCheckSettings{
			TransmitInterval: 50 * time.Millisecond,
			ReceiveInterval:  50 * time.Millisecond,
			DetectMultiplier: 3,
		},
		onChange: func(peer string, newState, oldState SessionState) {
			stateChanges <- newState
		},
		conn: conn,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s.start(ctx)
	defer s.stop()

	// Send 1 reply -- still Down.
	s.receiveReply(makeReply("peer-consec"))

	status := s.status()
	if status.State != StateDown {
		t.Errorf("expected Down after 1 reply, got %s", status.State)
	}

	if status.ConsecutiveReplies != 1 {
		t.Errorf("expected ConsecutiveReplies=1, got %d", status.ConsecutiveReplies)
	}

	// Send 2nd reply -- still Down.
	s.receiveReply(makeReply("peer-consec"))

	status = s.status()
	if status.State != StateDown {
		t.Errorf("expected Down after 2 replies, got %s", status.State)
	}

	if status.ConsecutiveReplies != 2 {
		t.Errorf("expected ConsecutiveReplies=2, got %d", status.ConsecutiveReplies)
	}

	// No state change should have occurred yet.
	select {
	case state := <-stateChanges:
		t.Fatalf("unexpected state change to %s before reaching DetectMultiplier", state)
	case <-time.After(50 * time.Millisecond):
	}

	// Send 3rd reply -- should transition to Up.
	s.receiveReply(makeReply("peer-consec"))

	waitForSessionState(t, stateChanges, StateUp, time.Second, "3rd reply")

	status = s.status()
	if status.ConsecutiveReplies != 3 {
		t.Errorf("expected ConsecutiveReplies=3 after going Up, got %d", status.ConsecutiveReplies)
	}
}

func TestConsecutiveRepliesResetOnTimeout(t *testing.T) {
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("ListenPacket: %v", err)
	}

	defer func() { _ = conn.Close() }()

	stateChanges := make(chan SessionState, 10)

	s := newSession(sessionConfig{
		peerHostname:  "peer-reset",
		overlayIP:     net.ParseIP("127.0.0.1"),
		port:          conn.LocalAddr().(*net.UDPAddr).Port,
		localHostname: "local",
		settings: HealthCheckSettings{
			TransmitInterval: 50 * time.Millisecond,
			ReceiveInterval:  50 * time.Millisecond,
			DetectMultiplier: 3,
		},
		onChange: func(peer string, newState, oldState SessionState) {
			stateChanges <- newState
		},
		conn: conn,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s.start(ctx)
	defer s.stop()

	// Send 2 replies (not enough for Up; need 3).
	sendNReplies(s, "peer-reset", 2)

	status := s.status()
	if status.State != StateDown {
		t.Fatalf("expected Down after 2 replies, got %s", status.State)
	}

	if status.ConsecutiveReplies != 2 {
		t.Errorf("expected ConsecutiveReplies=2, got %d", status.ConsecutiveReplies)
	}

	// No state change.
	select {
	case state := <-stateChanges:
		t.Fatalf("unexpected state change to %s", state)
	case <-time.After(50 * time.Millisecond):
	}

	// Wait for the detect loop to reset consecutive replies.
	// Timeout = 3 * 50ms = 150ms; detect loop fires every 100ms.
	// After ~300ms the counter should be reset.
	time.Sleep(400 * time.Millisecond)

	status = s.status()
	if status.ConsecutiveReplies != 0 {
		t.Errorf("expected ConsecutiveReplies=0 after timeout, got %d", status.ConsecutiveReplies)
	}

	// Now send 3 more replies and verify it takes all 3 to go Up.
	sendNReplies(s, "peer-reset", 2)

	select {
	case state := <-stateChanges:
		t.Fatalf("unexpected state change to %s after only 2 new replies", state)
	case <-time.After(50 * time.Millisecond):
	}

	s.receiveReply(makeReply("peer-reset"))
	waitForSessionState(t, stateChanges, StateUp, time.Second, "3rd new reply")
}

func TestFlapBackoffScaling(t *testing.T) {
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("ListenPacket: %v", err)
	}

	defer func() { _ = conn.Close() }()

	stateChanges := make(chan SessionState, 20)

	s := newSession(sessionConfig{
		peerHostname:  "peer-flapscale",
		overlayIP:     net.ParseIP("127.0.0.1"),
		port:          conn.LocalAddr().(*net.UDPAddr).Port,
		localHostname: "local",
		settings: HealthCheckSettings{
			TransmitInterval: 100 * time.Millisecond,
			ReceiveInterval:  100 * time.Millisecond,
			DetectMultiplier: 2,
			MaxBackoff:       10 * time.Second,
		},
		onChange: func(peer string, newState, oldState SessionState) {
			stateChanges <- newState
		},
		conn: conn,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s.start(ctx)
	defer s.stop()

	prevRequired := s.status().RequiredReplies

	for cycle := 0; cycle < 4; cycle++ {
		// Send enough replies to go Up.
		required := s.status().RequiredReplies
		sendNReplies(s, "peer-flapscale", required)
		waitForSessionState(t, stateChanges, StateUp, 2*time.Second, "flap cycle Up")

		// Wait for timeout -> Down.
		waitForSessionState(t, stateChanges, StateDown, 2*time.Second, "flap cycle Down")

		newRequired := s.status().RequiredReplies
		if cycle > 0 && newRequired <= prevRequired {
			t.Errorf("cycle %d: RequiredReplies did not increase: %d <= %d", cycle, newRequired, prevRequired)
		}

		prevRequired = newRequired
	}

	// Verify the cap: MaxBackoff / TransmitInterval = 10s / 100ms = 100.
	// Inject many flap timestamps to force requiredUpCount to the cap.
	s.mu.Lock()

	now := time.Now()
	for i := 0; i < 200; i++ {
		s.flapTimestamps = append(s.flapTimestamps, now)
	}
	s.mu.Unlock()

	status := s.status()

	expectedCap := int(10 * time.Second / (100 * time.Millisecond)) // 100
	if status.RequiredReplies != expectedCap {
		t.Errorf("expected RequiredReplies capped at %d, got %d", expectedCap, status.RequiredReplies)
	}
}

func TestFlapWindowExpiry(t *testing.T) {
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("ListenPacket: %v", err)
	}

	defer func() { _ = conn.Close() }()

	stateChanges := make(chan SessionState, 20)

	s := newSession(sessionConfig{
		peerHostname:  "peer-flapexpiry",
		overlayIP:     net.ParseIP("127.0.0.1"),
		port:          conn.LocalAddr().(*net.UDPAddr).Port,
		localHostname: "local",
		settings: HealthCheckSettings{
			TransmitInterval: 50 * time.Millisecond,
			ReceiveInterval:  50 * time.Millisecond,
			DetectMultiplier: 2,
			MaxBackoff:       10 * time.Second,
		},
		onChange: func(peer string, newState, oldState SessionState) {
			stateChanges <- newState
		},
		conn: conn,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s.start(ctx)
	defer s.stop()

	// Cause a couple of flaps.
	for i := 0; i < 2; i++ {
		required := s.status().RequiredReplies
		sendNReplies(s, "peer-flapexpiry", required)
		waitForSessionState(t, stateChanges, StateUp, 2*time.Second, "flapexpiry Up")
		waitForSessionState(t, stateChanges, StateDown, 2*time.Second, "flapexpiry Down")
	}

	status := s.status()
	if status.FlapCount == 0 {
		t.Fatal("expected FlapCount > 0 after flapping")
	}

	initialFlaps := status.FlapCount

	// Manually set all flap timestamps to be older than the 5-minute window.
	s.mu.Lock()

	old := time.Now().Add(-6 * time.Minute)
	for i := range s.flapTimestamps {
		s.flapTimestamps[i] = old
	}
	s.mu.Unlock()

	// Trigger another state change to invoke trimFlapTimestamps.
	// After trimming, only the new flap entry should remain.
	required := s.status().RequiredReplies
	sendNReplies(s, "peer-flapexpiry", required)
	waitForSessionState(t, stateChanges, StateUp, 2*time.Second, "flapexpiry post-trim Up")

	status = s.status()
	if status.FlapCount >= initialFlaps {
		t.Errorf("expected FlapCount to decrease after window expiry: was %d, now %d",
			initialFlaps, status.FlapCount)
	}
	// Should be exactly 1 (the new Up transition we just triggered).
	if status.FlapCount != 1 {
		t.Errorf("expected FlapCount=1 after trim, got %d", status.FlapCount)
	}
}

func TestDownTransitionNotAffectedByFlaps(t *testing.T) {
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("ListenPacket: %v", err)
	}

	defer func() { _ = conn.Close() }()

	stateChanges := make(chan SessionState, 20)

	s := newSession(sessionConfig{
		peerHostname:  "peer-downflap",
		overlayIP:     net.ParseIP("127.0.0.1"),
		port:          conn.LocalAddr().(*net.UDPAddr).Port,
		localHostname: "local",
		settings: HealthCheckSettings{
			TransmitInterval: 50 * time.Millisecond,
			ReceiveInterval:  50 * time.Millisecond,
			DetectMultiplier: 2,
			MaxBackoff:       10 * time.Second,
		},
		onChange: func(peer string, newState, oldState SessionState) {
			stateChanges <- newState
		},
		conn: conn,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s.start(ctx)
	defer s.stop()

	// Inject many flap timestamps so requiredUpCount is high.
	s.mu.Lock()

	now := time.Now()
	for i := 0; i < 50; i++ {
		s.flapTimestamps = append(s.flapTimestamps, now)
	}
	s.mu.Unlock()

	status := s.status()
	if status.RequiredReplies <= 2 {
		t.Fatalf("expected high RequiredReplies, got %d", status.RequiredReplies)
	}

	// Send enough replies to go Up.
	required := status.RequiredReplies
	sendNReplies(s, "peer-downflap", required)
	waitForSessionState(t, stateChanges, StateUp, 2*time.Second, "downflap Up")

	// Verify Down happens within the normal detect timeout, not delayed by flaps.
	// Timeout = DetectMultiplier(2) * max(50ms, 50ms) = 100ms.
	// DetectLoop checks every max(50ms, 100ms) = 100ms.
	// Expect Down within ~200-300ms.
	start := time.Now()

	select {
	case state := <-stateChanges:
		elapsed := time.Since(start)

		if state != StateDown {
			t.Errorf("expected StateDown, got %s", state)
		}

		if elapsed > 500*time.Millisecond {
			t.Errorf("Down transition took %v; expected within normal detect timeout (~200ms)", elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Down transition")
	}
}
