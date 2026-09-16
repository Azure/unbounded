// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package healthcheck

import (
	"context"
	"fmt"
	"net"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"google.golang.org/protobuf/proto"

	pb "github.com/Azure/unbounded/internal/net/healthcheck/proto"
)

var intervalEndProbePhaseSeed = ^uint64(0)

func TestProbePhaseBoundsAndIdentitySpread(t *testing.T) {
	for _, interval := range []time.Duration{1, 2, 15 * time.Second, 60 * time.Second, time.Duration(1<<63 - 1)} {
		for _, seed := range []uint64{0, 1, 1 << 63, ^uint64(0)} {
			s := newSession(sessionConfig{probePhaseSeed: &seed})

			phase := s.probePhase(interval)
			if phase <= 0 || phase > interval {
				t.Fatalf("seed=%d interval=%v phase=%v", seed, interval, phase)
			}

			if seed == 0 && phase != time.Nanosecond {
				t.Fatal("minimum phase must be positive")
			}

			if seed == ^uint64(0) && phase != interval {
				t.Fatal("maximum injected fraction must reach the interval boundary")
			}
		}
	}

	if peerProbePhaseSeed("ab", "c") == peerProbePhaseSeed("a", "bc") ||
		peerProbePhaseSeed("node-a", "node-b") == peerProbePhaseSeed("node-b", "node-a") {
		t.Fatal("phase identities are ambiguous or not directional")
	}

	for _, incoming := range []bool{false, true} {
		buckets := make([]int, 60)

		for i := range 2000 {
			local, remote := "node-fixed", fmt.Sprintf("node-%d", i)
			if incoming {
				local, remote = remote, local
			}

			s := newSession(sessionConfig{localHostname: local, peerHostname: remote})
			phase := s.probePhase(time.Minute)

			buckets[int((phase-1)/time.Second)]++
			if phase != newSession(sessionConfig{localHostname: local, peerHostname: remote}).probePhase(time.Minute) {
				t.Fatal("identity-derived phase is not deterministic")
			}
		}

		for second, count := range buckets {
			if count == 0 || count > 80 {
				t.Fatalf("incoming=%t second=%d count=%d: phases concentrate peers", incoming, second, count)
			}
		}

		minimum, maximum := 2000, 0
		for _, count := range buckets {
			minimum, maximum = min(minimum, count), max(maximum, count)
		}

		t.Logf("incoming=%t: 2000 peers span all 60 one-second buckets; min=%d max=%d", incoming, minimum, maximum)
	}
}

type phaseRecordingConn struct {
	discardProbeConn
	mu     sync.Mutex
	writes map[string][]time.Time
}

func (c *phaseRecordingConn) WriteTo(data []byte, _ net.Addr) (int, error) {
	var packet pb.HealthCheckPacket
	if err := proto.Unmarshal(data, &packet); err != nil {
		return 0, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	key := packet.SourceHostname + "|" + packet.DestinationHostname
	c.writes[key] = append(c.writes[key], time.Now())

	return len(data), nil
}

func TestProbePhasesSpreadFleetUpdatesAndPreserveRate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		conn := &phaseRecordingConn{writes: make(map[string][]time.Time)}
		settings := DefaultSettings()
		settings.TransmitInterval, settings.ReceiveInterval = time.Minute, time.Minute
		start := time.Now()

		var sessions []*session

		for i := range 128 {
			s := newSession(sessionConfig{localHostname: "local", peerHostname: fmt.Sprintf("peer-%d", i), settings: settings, conn: conn})
			s.start(ctx)
			sessions = append(sessions, s)
		}

		defer func() {
			cancel()

			for _, s := range sessions {
				s.stop()
			}
		}()

		synctest.Wait()
		time.Sleep(2 * time.Minute)
		synctest.Wait()

		buckets := make(map[int]int)

		for _, writes := range conn.writes {
			if len(writes) != 2 || writes[1].Sub(writes[0]) != time.Minute {
				t.Fatalf("steady cadence changed: %v", writes)
			}

			buckets[int(writes[0].Sub(start)/time.Second)]++
		}

		if len(conn.writes) != len(sessions) || len(buckets) < 30 {
			t.Fatalf("initial phases concentrated: peers=%d buckets=%d", len(conn.writes), len(buckets))
		}

		for _, count := range buckets {
			if count > 10 {
				t.Fatalf("initial one-second burst contains %d/128 peers", count)
			}
		}

		t.Logf("128 initial sessions: %d one-second buckets, exactly one probe per 60s interval", len(buckets))

		changedAt := time.Now()

		for _, s := range sessions {
			for _, interval := range []time.Duration{15 * time.Second, time.Minute, 15 * time.Second} {
				settings.TransmitInterval, settings.ReceiveInterval = interval, interval
				s.updateSettings(settings)
			}
		}

		synctest.Wait()
		time.Sleep(30 * time.Second)
		synctest.Wait()

		buckets = make(map[int]int)

		for _, writes := range conn.writes {
			if len(writes) != 4 || writes[3].Sub(writes[2]) != 15*time.Second {
				t.Fatalf("coalesced update changed steady cadence: %v", writes)
			}

			phase := writes[2].Sub(changedAt)
			if phase <= 0 || phase > 15*time.Second {
				t.Fatalf("updated phase out of bounds: %v", phase)
			}

			buckets[int(phase/time.Second)]++
		}

		if len(buckets) < 12 {
			t.Fatalf("reconfiguration phases concentrated in %d seconds", len(buckets))
		}

		t.Logf("128 reconfigured sessions: %d one-second buckets, exactly one probe per 15s interval", len(buckets))
		cancel()

		for _, s := range sessions {
			s.stop()
		}

		time.Sleep(time.Minute)

		for _, writes := range conn.writes {
			if len(writes) != 4 {
				t.Fatal("stopped sessions leaked probes")
			}
		}
	})
}

func TestProbePhaseCancellationAndNonTransmitUpdates(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		settings := DefaultSettings()
		settings.TransmitInterval = time.Minute
		s := newSession(sessionConfig{settings: settings, conn: &discardProbeConn{}, probePhaseSeed: &intervalEndProbePhaseSeed})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		s.start(ctx)
		defer s.stop()

		synctest.Wait()
		time.Sleep(10 * time.Second)

		settings.ReceiveInterval = 2 * time.Minute
		s.updateSettings(settings)
		settings.MaxBackoff = 240 * time.Second
		s.updateSettings(settings)
		s.updateSettings(settings)
		synctest.Wait()
		time.Sleep(50 * time.Second)
		synctest.Wait()

		if s.status().PacketsSent != 1 {
			t.Fatal("receive/backoff/unchanged settings disturbed the original transmit phase")
		}

		settings.TransmitInterval = time.Hour
		s.updateSettings(settings)
		synctest.Wait()
		cancel()
		s.stop()
		time.Sleep(2 * time.Hour)

		if s.status().PacketsSent != 1 {
			t.Fatal("canceled initial phase sent probes")
		}
	})
}

func TestShortenedIntervalsAllowFirstProbeNominalTimeout(t *testing.T) {
	for _, reply := range []bool{false, true} {
		t.Run(fmt.Sprintf("reply-%t", reply), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				settings := DefaultSettings()
				settings.TransmitInterval, settings.ReceiveInterval = time.Minute, time.Minute
				s := newSession(sessionConfig{settings: settings, conn: &discardProbeConn{}, probePhaseSeed: &intervalEndProbePhaseSeed})
				s.state, s.stateSince = StateUp, time.Now().Add(-time.Hour)
				s.lastReceived = time.Now().Add(-50 * time.Second)
				oldReply := s.lastReceived
				s.packetsReceived = 10

				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()

				s.start(ctx)
				defer s.stop()

				synctest.Wait()

				changedAt := time.Now()

				s.updateSettings(DefaultSettings())
				synctest.Wait()

				if s.detectTimeout() != 45*time.Second || s.detectGraceUntil.Sub(changedAt) != time.Minute {
					t.Fatal("nominal timeout or bounded transition window changed")
				}

				time.Sleep(23 * time.Second)
				synctest.Wait()

				if s.status().State != StateUp || s.packetsReceived != 10 || !s.lastReceived.Equal(oldReply) {
					t.Fatal("shortening applied the new timeout to an old-cadence reply or fabricated liveness")
				}

				if reply {
					s.receiveReply(&pb.HealthCheckPacket{TimestampNs: time.Now().UnixNano()})
					synctest.Wait()

					if !s.detectGraceUntil.IsZero() {
						t.Fatal("fresh reply did not restore ordinary detection")
					}

					time.Sleep(23 * time.Second)
					synctest.Wait()

					if s.status().State != StateUp {
						t.Fatal("healthy peer flapped during interval shortening")
					}

					time.Sleep(45 * time.Second)
				} else {
					time.Sleep(45 * time.Second)
				}

				synctest.Wait()

				if s.status().State != StateDown {
					t.Fatal("transition protection silently disabled failure detection")
				}
			})
		})
	}
}

func TestTimeoutTransitionRechecksFreshReply(t *testing.T) {
	settings := DefaultSettings()
	s := newSession(sessionConfig{settings: settings})
	s.state = StateUp
	s.lastReceived = time.Now().Add(-time.Minute)
	s.mu.Lock()
	expired := s.replyTimedOut(time.Now())
	s.mu.Unlock()

	if !expired {
		t.Fatal("test requires expired old snapshot")
	}

	s.receiveReply(&pb.HealthCheckPacket{TimestampNs: time.Now().UnixNano()})

	if s.setStateIf(StateDown, func() bool { return s.state == StateUp && s.replyTimedOut(time.Now()) }) {
		t.Fatal("stale timeout snapshot overrode a fresh reply")
	}
}
