// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package healthcheck

import (
	"context"
	"io"
	"net"
	"reflect"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	pb "github.com/Azure/unbounded/internal/net/healthcheck/proto"
)

type discardProbeConn struct{}

func (*discardProbeConn) ReadFrom([]byte) (int, net.Addr, error)    { return 0, nil, io.EOF }
func (*discardProbeConn) WriteTo(b []byte, _ net.Addr) (int, error) { return len(b), nil }
func (*discardProbeConn) Close() error                              { return nil }
func (*discardProbeConn) LocalAddr() net.Addr                       { return &net.UDPAddr{} }
func (*discardProbeConn) SetDeadline(time.Time) error               { return nil }
func (*discardProbeConn) SetReadDeadline(time.Time) error           { return nil }
func (*discardProbeConn) SetWriteDeadline(time.Time) error          { return nil }

func TestDefaultHealthCheckIntervals(t *testing.T) {
	settings := DefaultSettings()
	if settings.TransmitInterval != 15*time.Second || settings.ReceiveInterval != 15*time.Second ||
		settings.DetectMultiplier != 3 || settings.MaxBackoff != 120*time.Second {
		t.Fatalf("unexpected defaults: %+v", settings)
	}

	s := newSession(sessionConfig{settings: settings})
	if s.detectTimeout() != 45*time.Second {
		t.Fatal("unexpected default detection timeout")
	}
}

func TestManagerLiveSettingsPreserveHealthySession(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, err := NewManager("local", 0, nil)
		if err != nil {
			t.Fatal(err)
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		m.ctx, m.conn = ctx, &discardProbeConn{}
		defer m.Stop()

		ip := net.ParseIP("10.0.0.1")
		if err := m.AddPeer("peer", ip, DefaultSettings()); err != nil {
			t.Fatal(err)
		}

		synctest.Wait()

		s := m.sessions["peer"]

		time.Sleep(15 * time.Second)
		synctest.Wait()

		for range 3 {
			s.receiveReply(&pb.HealthCheckPacket{TimestampNs: time.Now().Add(-17 * time.Millisecond).UnixNano()})
		}

		synctest.Wait()

		before := s.status()
		if before.State != StateUp || before.PacketsSent != 1 || before.PacketsReceived != 3 || before.LastRTT != 17*time.Millisecond {
			t.Fatalf("session must start healthy with measurements: %+v", before)
		}

		for _, interval := range []time.Duration{60 * time.Second, 15 * time.Second, time.Second} {
			settings := DefaultSettings()

			settings.TransmitInterval, settings.ReceiveInterval = interval, interval
			if err := m.AddPeer("peer", ip, settings); err != nil {
				t.Fatal(err)
			}

			synctest.Wait()

			if m.sessions["peer"] != s {
				t.Fatal("settings-only AddPeer replaced session")
			}

			got, err := m.GetPeerSettings("peer")
			if err != nil || got != settings {
				t.Fatalf("applied settings: %+v, %v", got, err)
			}

			after := s.status()

			after.RequiredReplies = before.RequiredReplies
			if !reflect.DeepEqual(before, after) {
				t.Fatalf("health history reset: before=%+v after=%+v", before, after)
			}

			settings.MaxBackoff = 300 * time.Second
			if err := m.UpdatePeerSettings("peer", settings); err != nil {
				t.Fatal(err)
			}

			synctest.Wait()

			after = s.status()

			after.RequiredReplies = before.RequiredReplies
			if !reflect.DeepEqual(before, after) {
				t.Fatal("UpdatePeerSettings reset health history")
			}
		}
	})
}

func TestSessionSettingsWakeProbeTimer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		settings := DefaultSettings()
		settings.TransmitInterval, settings.ReceiveInterval = time.Hour, time.Hour
		s := newSession(sessionConfig{settings: settings, conn: &discardProbeConn{}, probePhaseSeed: &intervalEndProbePhaseSeed})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		s.start(ctx)
		defer s.stop()

		synctest.Wait()

		settings.TransmitInterval = 20 * time.Millisecond
		s.updateSettings(settings)
		synctest.Wait()
		time.Sleep(21 * time.Millisecond)
		synctest.Wait()

		if s.status().PacketsSent != 1 {
			t.Fatal("shortened interval waited for old one-hour timer")
		}

		settings.TransmitInterval = time.Hour
		s.updateSettings(settings)
		synctest.Wait()
		time.Sleep(time.Second)
		synctest.Wait()

		if s.status().PacketsSent != 1 {
			t.Fatal("lengthened interval allowed old fast probes")
		}

		for _, interval := range []time.Duration{10 * time.Millisecond, time.Hour, 20 * time.Millisecond} {
			settings.TransmitInterval = interval
			s.updateSettings(settings)
		}

		synctest.Wait()
		time.Sleep(21 * time.Millisecond)
		synctest.Wait()

		if s.status().PacketsSent != 2 {
			t.Fatal("coalesced updates did not use latest interval")
		}

		cancel()
		s.stop()
		count := s.status().PacketsSent
		s.updateSettings(DefaultSettings())
		time.Sleep(time.Minute)

		if s.status().PacketsSent != count {
			t.Fatal("stopped session resumed probes")
		}
	})
}

func TestSessionSettingsWakeDetectionTimer(t *testing.T) {
	for _, increase := range []bool{false, true} {
		t.Run(map[bool]string{false: "shorten", true: "lengthen"}[increase], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				settings := DefaultSettings()

				settings.TransmitInterval, settings.ReceiveInterval = time.Hour, time.Hour
				if increase {
					settings.TransmitInterval, settings.ReceiveInterval = 20*time.Millisecond, 20*time.Millisecond
				}

				s := newSession(sessionConfig{settings: settings, conn: &discardProbeConn{}, probePhaseSeed: &intervalEndProbePhaseSeed})
				s.state, s.lastReceived = StateUp, time.Now()

				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()

				s.start(ctx)
				defer s.stop()

				synctest.Wait()

				settings.TransmitInterval, settings.ReceiveInterval = 20*time.Millisecond, 20*time.Millisecond
				if increase {
					settings.TransmitInterval, settings.ReceiveInterval = time.Hour, time.Hour
				}

				s.updateSettings(settings)
				synctest.Wait()
				time.Sleep(201 * time.Millisecond)
				synctest.Wait()

				want := StateDown
				if increase {
					want = StateUp
				}

				if got := s.status().State; got != want {
					t.Fatalf("state=%v want %v after timer update", got, want)
				}
			})
		})
	}
}

func TestSessionIdenticalSettingsKeepProbeDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		settings := DefaultSettings()
		settings.TransmitInterval = 50 * time.Millisecond
		s := newSession(sessionConfig{settings: settings, conn: &discardProbeConn{}})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		s.start(ctx)
		defer s.stop()

		synctest.Wait()
		time.Sleep(20 * time.Millisecond)
		s.updateSettings(settings)
		synctest.Wait()
		time.Sleep(31 * time.Millisecond)
		synctest.Wait()

		if s.status().PacketsSent != 1 {
			t.Fatal("identical settings reset probe deadline")
		}
	})
}

func TestManagerIPReplacementJoinsSessionOutsideLookupLock(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		entered, release := make(chan struct{}), make(chan struct{})

		var (
			m   *Manager
			err error
		)

		m, err = NewManager("local", 0, func(string, SessionState, SessionState) {
			close(entered)
			<-release
			m.GetAllPeerStatuses()
		})
		if err != nil {
			t.Fatal(err)
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		m.ctx, m.conn = ctx, &discardProbeConn{}
		defer m.Stop()

		if err := m.AddPeer("peer", net.ParseIP("10.0.0.1"), DefaultSettings()); err != nil {
			t.Fatal(err)
		}

		old := m.sessions["peer"]
		old.setState(StateUp)
		<-entered

		done := make(chan error, 1)

		go func() { done <- m.AddPeer("peer", net.ParseIP("10.0.0.2"), DefaultSettings()) }()

		synctest.Wait()
		close(release)

		if err := <-done; err != nil {
			t.Fatal(err)
		}

		if m.sessions["peer"] == old || m.sessions["peer"].status().State != StateDown {
			t.Fatal("IP replacement retained old session")
		}

		synctest.Wait()

		before := old.status().PacketsSent

		time.Sleep(16 * time.Second)
		synctest.Wait()

		if old.status().PacketsSent != before {
			t.Fatal("old IP session still sends probes")
		}
	})
}

func TestManagerConcurrentSettingsAndStatus(t *testing.T) {
	m, err := NewManager("local", 0, nil)
	if err != nil {
		t.Fatal(err)
	}

	ip := net.ParseIP("10.0.0.1")
	if err := m.AddPeer("peer", ip, DefaultSettings()); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for worker := range 4 {
		wg.Go(func() {
			for i := range 100 {
				settings := DefaultSettings()
				settings.TransmitInterval += time.Duration(i) * time.Millisecond

				var err error

				switch worker {
				case 0:
					err = m.AddPeer("peer", ip, settings)
				case 1:
					err = m.UpdatePeerSettings("peer", settings)
				case 2:
					_, err = m.GetPeerSettings("peer")
				case 3:
					_, err = m.GetPeerStatus("peer")
				}

				if err != nil {
					t.Errorf("concurrent operation: %v", err)
					return
				}
			}
		})
	}

	wg.Wait()
}

func TestManagerRejectsInvalidSettings(t *testing.T) {
	for name, change := range map[string]func(*HealthCheckSettings){
		"tx":               func(s *HealthCheckSettings) { s.TransmitInterval = 0 },
		"rx":               func(s *HealthCheckSettings) { s.ReceiveInterval = -1 },
		"multiplier":       func(s *HealthCheckSettings) { s.DetectMultiplier = 0 },
		"large multiplier": func(s *HealthCheckSettings) { s.DetectMultiplier = 256 },
		"backoff":          func(s *HealthCheckSettings) { s.MaxBackoff = -1 },
		"overflow":         func(s *HealthCheckSettings) { s.ReceiveInterval = time.Duration(1<<63 - 1) },
	} {
		t.Run(name, func(t *testing.T) {
			m, err := NewManager("local", 0, nil)
			if err != nil {
				t.Fatal(err)
			}

			settings := DefaultSettings()
			change(&settings)

			if err := m.AddPeer("peer", net.ParseIP("10.0.0.1"), settings); err == nil {
				t.Fatal("invalid settings accepted")
			}

			if len(m.sessions) != 0 {
				t.Fatal("invalid AddPeer mutated sessions")
			}

			if err := m.AddPeer("peer", net.ParseIP("10.0.0.1"), DefaultSettings()); err != nil {
				t.Fatal(err)
			}

			if err := m.UpdatePeerSettings("peer", settings); err == nil {
				t.Fatal("invalid update accepted")
			}

			if got, _ := m.GetPeerSettings("peer"); got != DefaultSettings() {
				t.Fatal("invalid update changed settings")
			}
		})
	}
}
