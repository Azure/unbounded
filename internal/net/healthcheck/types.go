// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package healthcheck

import (
	"fmt"
	"time"
)

// SessionState represents the health state of a peer session.
type SessionState int

const (
	// StateDown indicates the peer is unreachable.
	StateDown SessionState = iota
	// StateUp indicates the peer is healthy and responding.
	StateUp
	// StateAdminDown indicates the session has been administratively disabled.
	StateAdminDown
)

// String returns a human-readable session state.
func (s SessionState) String() string {
	switch s {
	case StateDown:
		return "Down"
	case StateUp:
		return "Up"
	case StateAdminDown:
		return "AdminDown"
	default:
		return "Unknown"
	}
}

// HealthCheckSettings contains the configuration for a health check session.
type HealthCheckSettings struct {
	TransmitInterval time.Duration
	ReceiveInterval  time.Duration
	DetectMultiplier int
	MaxBackoff       time.Duration
}

// DefaultSettings returns the default health check settings.
func DefaultSettings() HealthCheckSettings {
	return HealthCheckSettings{
		TransmitInterval: 15 * time.Second,
		ReceiveInterval:  15 * time.Second,
		DetectMultiplier: 3,
		MaxBackoff:       120 * time.Second,
	}
}

func (s HealthCheckSettings) validate() error {
	if s.TransmitInterval <= 0 || s.ReceiveInterval <= 0 {
		return fmt.Errorf("health check intervals must be positive")
	}

	if s.DetectMultiplier < 1 || s.DetectMultiplier > 255 {
		return fmt.Errorf("health check detect multiplier must be between 1 and 255")
	}

	const maxDuration = time.Duration(1<<63 - 1)
	if max(s.TransmitInterval, s.ReceiveInterval) > maxDuration/time.Duration(s.DetectMultiplier) {
		return fmt.Errorf("health check detection timeout overflows time.Duration")
	}

	if s.MaxBackoff < 0 {
		return fmt.Errorf("health check maximum backoff must not be negative")
	}

	return nil
}

// PeerStatus contains the current health status of a peer.
type PeerStatus struct {
	State              SessionState
	Since              time.Time
	LastRTT            time.Duration
	PacketsSent        uint64
	PacketsReceived    uint64
	ConsecutiveReplies int
	RequiredReplies    int
	FlapCount          int
}

// StateChangeFunc is called when a peer's health state changes.
type StateChangeFunc func(peerHostname string, newState, oldState SessionState)
