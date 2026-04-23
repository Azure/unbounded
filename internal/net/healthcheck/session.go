// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package healthcheck

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/proto"
	"k8s.io/klog/v2"

	pb "github.com/Azure/unbounded/internal/net/healthcheck/proto"
)

// stateEvent represents a session state transition to be delivered via the
// callback channel so that callbacks are serialized by callbackLoop.
type stateEvent struct {
	newState SessionState
	oldState SessionState
}

// flapWindowDuration is the sliding window over which state transitions
// (flaps) are counted for backoff calculation.
const flapWindowDuration = 5 * time.Minute

// session tracks the health state of a single remote peer.
// It sends periodic probes and monitors for replies to determine
// if the peer is up.
type session struct {
	peerHostname string
	overlayIP    net.IP
	port         int
	localHost    string

	mu                 sync.Mutex
	settings           HealthCheckSettings
	state              SessionState
	stateSince         time.Time
	lastReceived       time.Time
	lastRTT            time.Duration
	packetsSent        uint64
	packetsReceived    uint64
	consecutiveReplies int
	flapTimestamps     []time.Time
	seqNum             atomic.Uint64
	onStateChange      StateChangeFunc

	conn       net.PacketConn
	cancel     context.CancelFunc
	callbackCh chan stateEvent
	wg         sync.WaitGroup
	started    bool
}

// sessionConfig holds the parameters needed to create a new session.
type sessionConfig struct {
	peerHostname  string
	overlayIP     net.IP
	port          int
	localHostname string
	settings      HealthCheckSettings
	onChange      StateChangeFunc
	conn          net.PacketConn
}

// newSession creates a new health check session for a remote peer.
func newSession(cfg sessionConfig) *session {
	now := time.Now()

	return &session{
		peerHostname:  cfg.peerHostname,
		overlayIP:     cfg.overlayIP,
		port:          cfg.port,
		localHost:     cfg.localHostname,
		settings:      cfg.settings,
		state:         StateDown,
		stateSince:    now,
		onStateChange: cfg.onChange,
		conn:          cfg.conn,
		callbackCh:    make(chan stateEvent, 8),
	}
}

// start begins sending probes and monitoring for timeouts.
func (s *session) start(ctx context.Context) {
	ctx, s.cancel = context.WithCancel(ctx)
	s.started = true
	s.wg.Add(3)

	go s.probeLoop(ctx)
	go s.detectLoop(ctx)
	go s.callbackLoop(ctx)
}

// stop halts probe sending and detection.
func (s *session) stop() {
	if s.cancel != nil {
		s.cancel()
	}

	if s.started {
		s.wg.Wait()
	}
}

// receiveReply processes a reply packet from the peer.
func (s *session) receiveReply(pkt *pb.HealthCheckPacket) {
	now := time.Now()

	rtt := time.Duration(now.UnixNano()-pkt.TimestampNs) * time.Nanosecond
	if rtt < 0 {
		rtt = 0
	}

	s.mu.Lock()
	s.lastReceived = now
	s.lastRTT = rtt
	s.packetsReceived++

	oldState := s.state
	if oldState == StateDown {
		s.consecutiveReplies++
	}

	threshold := s.requiredUpCount()
	replies := s.consecutiveReplies
	s.mu.Unlock()

	metricPacketsReceived.Inc()
	metricRTT.Observe(rtt.Seconds())

	if oldState == StateDown && replies >= threshold {
		s.setState(StateUp)
	}
}

// status returns the current peer status.
func (s *session) status() *PeerStatus {
	s.mu.Lock()
	defer s.mu.Unlock()

	return &PeerStatus{
		State:              s.state,
		Since:              s.stateSince,
		LastRTT:            s.lastRTT,
		PacketsSent:        s.packetsSent,
		PacketsReceived:    s.packetsReceived,
		ConsecutiveReplies: s.consecutiveReplies,
		RequiredReplies:    s.requiredUpCount(),
		FlapCount:          len(s.flapTimestamps),
	}
}

// updateSettings applies new health check settings.
func (s *session) updateSettings(settings HealthCheckSettings) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.settings = settings
}

func (s *session) probeLoop(ctx context.Context) {
	defer s.wg.Done()

	s.mu.Lock()
	interval := s.settings.TransmitInterval
	s.mu.Unlock()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.sendProbe()
			// Check if interval changed.
			s.mu.Lock()
			newInterval := s.settings.TransmitInterval
			s.mu.Unlock()

			if newInterval != interval {
				interval = newInterval
				ticker.Reset(interval)
			}
		}
	}
}

func (s *session) detectLoop(ctx context.Context) {
	defer s.wg.Done()
	// Check at half the detect timeout for faster detection.
	s.mu.Lock()
	timeout := s.detectTimeout()
	s.mu.Unlock()

	checkInterval := timeout / 2
	if checkInterval < 100*time.Millisecond {
		checkInterval = 100 * time.Millisecond
	}

	ticker := time.NewTicker(checkInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.mu.Lock()
			state := s.state
			lastRecv := s.lastReceived
			timeout = s.detectTimeout()
			// Reset consecutive replies counter when we detect a timeout,
			// whether currently Up (transitioning to Down) or already Down
			// (stale counter from a partial reply burst).
			if !lastRecv.IsZero() && time.Since(lastRecv) > timeout {
				s.consecutiveReplies = 0
			}
			s.mu.Unlock()

			if state == StateAdminDown {
				continue
			}

			if state == StateUp && !lastRecv.IsZero() && time.Since(lastRecv) > timeout {
				metricPacketsTimeout.Inc()
				s.setState(StateDown)
			}

			// Update check interval if settings changed.
			newCheck := timeout / 2
			if newCheck < 100*time.Millisecond {
				newCheck = 100 * time.Millisecond
			}

			if newCheck != checkInterval {
				checkInterval = newCheck
				ticker.Reset(checkInterval)
			}
		}
	}
}

func (s *session) detectTimeout() time.Duration {
	tx := s.settings.TransmitInterval
	rx := s.settings.ReceiveInterval

	maxInterval := tx
	if rx > maxInterval {
		maxInterval = rx
	}

	return time.Duration(s.settings.DetectMultiplier) * maxInterval
}

func (s *session) sendProbe() {
	seq := s.seqNum.Add(1)
	pkt := &pb.HealthCheckPacket{
		SourceHostname:      s.localHost,
		DestinationHostname: s.peerHostname,
		SequenceNumber:      seq,
		TimestampNs:         time.Now().UnixNano(),
		Type:                pb.PacketType_REQUEST,
	}

	data, err := proto.Marshal(pkt)
	if err != nil {
		klog.Errorf("healthcheck: failed to marshal probe for %s: %v", s.peerHostname, err)
		return
	}

	s.mu.Lock()
	addr := &net.UDPAddr{IP: s.overlayIP, Port: s.port}
	s.mu.Unlock()

	if _, err := s.conn.WriteTo(data, addr); err != nil {
		klog.V(4).Infof("healthcheck: failed to send probe to %s (%s): %v",
			s.peerHostname, addr, err)

		return
	}

	s.mu.Lock()
	s.packetsSent++
	s.mu.Unlock()
	metricPacketsSent.Inc()
}

func (s *session) setState(newState SessionState) {
	s.mu.Lock()

	oldState := s.state
	if oldState == newState {
		s.mu.Unlock()
		return
	}

	s.state = newState

	s.stateSince = time.Now()
	if newState == StateDown {
		s.consecutiveReplies = 0
	}
	// Track the flap and trim entries outside the sliding window.
	now := time.Now()
	s.flapTimestamps = append(s.flapTimestamps, now)
	s.trimFlapTimestamps(now)
	s.mu.Unlock()

	klog.Infof("healthcheck: peer %s state %s -> %s", s.peerHostname, oldState, newState)
	metricSessionFlaps.Inc()

	select {
	case s.callbackCh <- stateEvent{newState: newState, oldState: oldState}:
	default:
		klog.V(2).Infof("healthcheck: callback channel full for peer %s, dropping %s -> %s",
			s.peerHostname, oldState, newState)
	}
}

// trimFlapTimestamps removes flap timestamps older than flapWindowDuration.
// Must be called with s.mu held.
func (s *session) trimFlapTimestamps(now time.Time) {
	cutoff := now.Add(-flapWindowDuration)

	i := 0
	for i < len(s.flapTimestamps) && s.flapTimestamps[i].Before(cutoff) {
		i++
	}

	if i > 0 {
		s.flapTimestamps = s.flapTimestamps[i:]
	}
}

// requiredUpCount computes the number of consecutive replies needed before
// transitioning from Down to Up, incorporating flap-based backoff.
// Must be called with s.mu held.
func (s *session) requiredUpCount() int {
	flapCount := len(s.flapTimestamps)
	base := s.settings.DetectMultiplier

	mult := flapCount
	if mult < 1 {
		mult = 1
	}

	required := base * mult

	maxBackoff := s.settings.MaxBackoff
	if maxBackoff <= 0 {
		maxBackoff = 120 * time.Second
	}

	maxAllowed := int(maxBackoff / s.settings.TransmitInterval)
	if maxAllowed < 1 {
		maxAllowed = 1
	}

	if required > maxAllowed {
		required = maxAllowed
	}

	return required
}

// callbackLoop serializes state-change callbacks so they are delivered in
// order and never concurrently. It re-checks the session state before
// invoking the callback to skip stale events.
func (s *session) callbackLoop(ctx context.Context) {
	defer s.wg.Done()

	for {
		select {
		case <-ctx.Done():
			// Drain remaining events before exiting.
			for {
				select {
				case evt := <-s.callbackCh:
					s.invokeCallback(evt)
				default:
					return
				}
			}
		case evt := <-s.callbackCh:
			s.invokeCallback(evt)
		}
	}
}

// invokeCallback delivers a single state-change event to the registered
// callback after confirming the event is still current.
func (s *session) invokeCallback(evt stateEvent) {
	s.mu.Lock()
	currentState := s.state
	onChange := s.onStateChange
	s.mu.Unlock()

	if evt.newState != currentState {
		klog.V(4).Infof("healthcheck: stale callback for peer %s: event %s -> %s, current state is %s",
			s.peerHostname, evt.oldState, evt.newState, currentState)

		return
	}

	if onChange != nil {
		onChange(s.peerHostname, evt.newState, evt.oldState)
	}
}
