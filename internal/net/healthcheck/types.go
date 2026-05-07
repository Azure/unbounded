// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package healthcheck

import "time"

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
		TransmitInterval: 1000 * time.Millisecond,
		ReceiveInterval:  1000 * time.Millisecond,
		DetectMultiplier: 3,
		MaxBackoff:       120 * time.Second,
	}
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
