// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package netlink

import (
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	wgPeerLastHandshakeDesc = prometheus.NewDesc(
		"unbounded_cni_node_wireguard_peer_last_handshake_seconds",
		"Seconds since epoch of the last WireGuard handshake with this peer.",
		[]string{"interface", "public_key"}, nil,
	)
	wgPeerRxBytesDesc = prometheus.NewDesc(
		"unbounded_cni_node_wireguard_peer_rx_bytes",
		"Total bytes received from this WireGuard peer.",
		[]string{"interface", "public_key"}, nil,
	)
	wgPeerTxBytesDesc = prometheus.NewDesc(
		"unbounded_cni_node_wireguard_peer_tx_bytes",
		"Total bytes transmitted to this WireGuard peer.",
		[]string{"interface", "public_key"}, nil,
	)
)

// WireGuardCollector is a Prometheus collector that queries active WireGuard
// managers for per-peer transfer and handshake statistics at scrape time.
type WireGuardCollector struct {
	mu       sync.RWMutex
	managers []*WireGuardManager
}

// NewWireGuardCollector creates a new collector. Register WireGuard managers
// with SetManagers as they are created.
func NewWireGuardCollector() *WireGuardCollector {
	return &WireGuardCollector{}
}

// SetManagers replaces the set of WireGuard managers to query on each scrape.
func (c *WireGuardCollector) SetManagers(managers []*WireGuardManager) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.managers = managers
}

// Describe implements prometheus.Collector.
func (c *WireGuardCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- wgPeerLastHandshakeDesc

	ch <- wgPeerRxBytesDesc

	ch <- wgPeerTxBytesDesc
}

// Collect implements prometheus.Collector. It queries each registered
// WireGuard manager and emits per-peer gauges.
func (c *WireGuardCollector) Collect(ch chan<- prometheus.Metric) {
	c.mu.RLock()
	managers := c.managers
	c.mu.RUnlock()

	for _, wm := range managers {
		device, err := wm.GetDevice()
		if err != nil {
			continue
		}

		ifaceName := device.Name
		for _, peer := range device.Peers {
			pubKey := peer.PublicKey.String()

			if !peer.LastHandshakeTime.IsZero() {
				ch <- prometheus.MustNewConstMetric(
					wgPeerLastHandshakeDesc,
					prometheus.GaugeValue,
					float64(peer.LastHandshakeTime.Unix()),
					ifaceName, pubKey,
				)
			}

			ch <- prometheus.MustNewConstMetric(
				wgPeerRxBytesDesc,
				prometheus.GaugeValue,
				float64(peer.ReceiveBytes),
				ifaceName, pubKey,
			)

			ch <- prometheus.MustNewConstMetric(
				wgPeerTxBytesDesc,
				prometheus.GaugeValue,
				float64(peer.TransmitBytes),
				ifaceName, pubKey,
			)
		}
	}
}

// LastHandshakeAge returns the duration since the last handshake for a peer,
// or 0 if the handshake time is zero.
func LastHandshakeAge(lastHandshake time.Time) time.Duration {
	if lastHandshake.IsZero() {
		return 0
	}

	return time.Since(lastHandshake)
}
