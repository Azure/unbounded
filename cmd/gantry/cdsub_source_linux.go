// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build linux

package main

import (
	"log/slog"

	"github.com/Azure/unbounded/internal/gantry/cdsub"
	"github.com/Azure/unbounded/internal/gantry/config"
	"github.com/Azure/unbounded/internal/gantry/containerdstore"
	"github.com/Azure/unbounded/internal/gantry/digest"
)

// newCdsubSource returns the production containerd-backed ImageSource
// when running on linux. In containerd-only mode the socket is required
// by config validation; returning NoOpSource here is only a typed failure
// sentinel so main can fail loudly when containerdBackedStore cannot be
// constructed.
func newCdsubSource(c *config.Config, logger *slog.Logger) cdsub.ImageSource {
	if c.ContainerdSocket == "" {
		logger.Info("cdsub: containerd_socket empty — running with NoOpSource")
		return cdsub.NoOpSource{}
	}
	src, err := cdsub.NewContainerdSource(c.ContainerdSocket, c.ContainerdNamespace,
		cdsub.WithContainerdLogger(logger),
	)
	if err != nil {
		logger.Warn("cdsub: containerd source unavailable, falling back to NoOpSource",
			slog.String("socket", c.ContainerdSocket),
			slog.String("namespace", c.ContainerdNamespace),
			slog.Any("err", err),
		)
		return cdsub.NoOpSource{}
	}
	return src
}

// containerdBackedStore returns a containerdstore.Store wrapped as
// ifaces.LocalContentStore when src is a real containerd connection.
// Used by main when StorageMode == "containerd" so the agent reads
// from and writes into the same content store containerd shows to
// kubelet. Returns nil for NoOpSource so main can detect the
// misconfiguration and fail loudly rather than silently run without
// a usable local content store.
//
// The returned Store also satisfies advertise.Inventory (it has an
// Inventory method) so main can pass the same instance to the
// advertiser without opening a second containerd connection.
//
// The lease manager from the same containerd connection is wired in
// (Plan §Phase 7) so background Gantry ingests can create a bounded
// lease before writing into the content store, without opening a
// second gRPC channel.
func containerdBackedStore(src cdsub.ImageSource, c *config.Config, extra ...containerdstore.Option) *containerdstore.Store {
	cs, ok := src.(*cdsub.ContainerdSource)
	if !ok {
		return nil
	}
	store := cs.ContentStore()
	if store == nil {
		return nil
	}
	opts := []containerdstore.Option{
		containerdstore.WithNamespace(cs.Namespace()),
	}
	if lm := cs.LeasesService(); lm != nil {
		opts = append(opts, containerdstore.WithLeaseManager(lm))
	}
	if c != nil && c.ContainerdLeaseTTL > 0 {
		opts = append(opts, containerdstore.WithLeaseTTL(c.ContainerdLeaseTTL))
	}
	opts = append(opts, extra...)
	return containerdstore.New(store, opts...)
}

// wireDescriptorRecorder plumbs cdsub walker → containerdstore
// descriptor index (plan §"Descriptor index"). On linux the source
// is the real containerd one; on non-linux dev builds it is
// NoOpSource and this becomes a no-op.
func wireDescriptorRecorder(src cdsub.ImageSource, store *containerdstore.Store) {
	if store == nil {
		return
	}
	cs, ok := src.(*cdsub.ContainerdSource)
	if !ok {
		return
	}
	cs.SetMediaTypeRecorder(func(d digest.Digest, mt string) {
		store.RememberMediaType(d, mt)
	})
}
