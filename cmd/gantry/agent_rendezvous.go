// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/Azure/unbounded/internal/gantry/config"
	"github.com/Azure/unbounded/internal/gantry/discovery"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/rendezvous"
)

func buildLeaseRendezvous(c *config.Config, disco *discovery.Host, logger *slog.Logger, metrics *rendezvousMetrics) (ifaces.NodeID, *rendezvous.Bootstrap, error) {
	var manager *rendezvous.Manager

	if c.Rendezvous.Namespace != "" {
		leases, err := rendezvous.NewLeaseClient(c.Rendezvous.Kubeconfig, c.Rendezvous.Namespace)
		if err != nil {
			return "", nil, err
		}

		manager, err = rendezvous.New(rendezvous.Options{
			Leases:                leases,
			PeerID:                disco.PeerID(),
			Addrs:                 disco.Addrs,
			SlotCount:             c.Rendezvous.SlotCount,
			ReadsPerRound:         c.Rendezvous.ReadsPerRound,
			ClaimAttemptsPerRound: c.Rendezvous.ClaimAttemptsPerRound,
			ContactsPerSlot:       c.Rendezvous.ContactsPerSlot,
			FullScanAfter:         c.Rendezvous.FullScanAfter,
			LeaseDuration:         c.Rendezvous.LeaseDuration,
			StaleContactGrace:     c.Rendezvous.StaleContactGrace,
			Logger:                logger,
			Metrics: rendezvous.Metrics{
				OnSlotGet:   func(outcome string) { metrics.slotGet.WithLabelValues(outcome).Inc() },
				OnSlotClaim: func(outcome string) { metrics.slotClaim.WithLabelValues(outcome).Inc() },
				OnSlotRenew: func(outcome string) { metrics.slotRenew.WithLabelValues(outcome).Inc() },
				OnContact:   func(freshness string) { metrics.contacts.WithLabelValues(freshness).Inc() },
				OnSlotHeld: func(held bool) {
					if held {
						metrics.slotHeld.Set(1)
					} else {
						metrics.slotHeld.Set(0)
					}
				},
			},
		})
		if err != nil {
			return "", nil, err
		}
	}

	bootstrap, err := rendezvous.NewBootstrap(rendezvous.BootstrapOptions{
		Manager: manager,
		PeerID:  disco.PeerID(),
		Connect: func(ctx context.Context, addresses []string) rendezvous.DialResult {
			result := disco.ConnectPeersDetailed(ctx, addresses)

			return rendezvous.DialResult{Attempted: result.Attempted, Connected: result.Connected}
		},
		RoutingTableSize: disco.RoutingTableSize,
		SelfTest:         disco.SelfTest,
		SingleNode:       c.Rendezvous.SingleNode,
		RetryMin:         c.Rendezvous.RetryMin,
		RetryMax:         c.Rendezvous.RetryMax,
		RenewInterval:    c.Rendezvous.RenewInterval,
		PeerCachePath:    c.Rendezvous.PeerCachePath,
		Logger:           logger,
		Metrics: rendezvous.BootstrapMetrics{
			OnDial:              func(outcome, source string) { metrics.dials.WithLabelValues(outcome, source).Inc() },
			OnBootstrapDuration: func(duration time.Duration) { metrics.bootstrapDuration.Observe(duration.Seconds()) },
			OnPeerCacheEntries:  func(entries int) { metrics.peerCacheEntries.Set(float64(entries)) },
		},
	})
	if err != nil {
		return "", nil, err
	}

	return ifaces.NodeID(disco.PeerID().String()), bootstrap, nil
}
