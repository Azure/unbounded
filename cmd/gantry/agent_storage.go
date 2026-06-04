// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"fmt"
	"log/slog"

	"github.com/Azure/unbounded/internal/gantry/advertise"
	"github.com/Azure/unbounded/internal/gantry/cdsub"
	"github.com/Azure/unbounded/internal/gantry/config"
	"github.com/Azure/unbounded/internal/gantry/containerdstore"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
)

// buildContainerdStorage constructs the agent's local content store
// (plan §Phase 8 — containerd content store is the only backend) and
// the related advertise.Inventory view. Wires the cdsub walker's
// (digest, mediaType) recorder into the store's descriptor index so
// the transfer endpoint can serve manifest replies with the right
// Content-Type without re-parsing manifest bodies.
//
// Returns (store, content-store-as-LocalContentStore,
// content-store-as-Inventory, err). The first return is the concrete
// type so callers that need Ping/Inventory/lease/MediaType lookups
// (readiness, transfer, lease cleanup loop) can call them; the second
// is the same value typed as the interface the mirror/transfer
// servers consume; the third is the inventory interface the
// advertiser consumes. All three values point at the same underlying
// containerd content store — no duplicate dial.
//
// An unavailable backend (NoOpSource on linux because the socket
// could not be dialed, or any non-linux dev build) is a hard error
// here: config.Validate has already required storage_mode=containerd
// and a non-empty containerd_socket, so falling back silently would
// ship an agent that pretends to be ready but reads from nothing.
func buildContainerdStorage(
	c *config.Config,
	src cdsub.ImageSource,
	logger *slog.Logger,
	p9 *phase9Metrics,
) (*containerdstore.Store, ifaces.LocalContentStore, advertise.Inventory, error) {
	cdstore := containerdBackedStore(src, c,
		containerdstore.WithMetrics(containerdstore.MetricsHooks{
			OnHit:         func() { p9.containerdHit.Inc() },
			OnMiss:        func() { p9.containerdMiss.Inc() },
			OnUnavailable: func() { p9.containerdUnavailable.Inc() },
			OnOpenError:   func() { p9.containerdOpenError.Inc() },
		}),
	)
	if cdstore == nil {
		return nil, nil, nil, fmt.Errorf("storage_mode=containerd selected but containerd content store is unavailable (check containerd_socket and platform — see plan §Phase 5)")
	}
	// Wire the cdsub walker → containerdstore descriptor-index
	// pipeline (plan §"Descriptor index"). The walker already has
	// (digest, mediaType) for every visited descriptor; forwarding
	// them now means the transfer endpoint's manifest replies can
	// set Content-Type without re-parsing the manifest body. The
	// platform shim handles the non-linux NoOpSource case (no-op).
	wireDescriptorRecorder(src, cdstore)
	logger.Info("storage backend: containerd content store",
		slog.String("namespace", c.ContainerdNamespace),
		slog.String("socket", c.ContainerdSocket),
	)
	return cdstore, cdstore, cdstore, nil
}
