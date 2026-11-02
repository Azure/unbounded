// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/Azure/unbounded/internal/gantry/coldstart"
	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
)

type streamingLayerReconcilerOptions struct {
	Resolver        streamingManifestPrefetchResolver
	Store           ifaces.LocalContentStore
	Seeder          *layerSeeder
	Logger          *slog.Logger
	Interval        time.Duration
	DispatchTimeout time.Duration
}

// streamingLayerReconciler owns durable whole-layer convergence for manifests
// identified as ACR Artifact Streaming images. Regular OCI manifests never
// enter this component and retain the existing request-driven pull behavior.
type streamingLayerReconciler struct {
	resolver        streamingManifestPrefetchResolver
	store           ifaces.LocalContentStore
	seeder          *layerSeeder
	logger          *slog.Logger
	interval        time.Duration
	dispatchTimeout time.Duration

	mu     sync.Mutex
	active map[digest.Digest]struct{}
}

func newStreamingLayerReconciler(opts streamingLayerReconcilerOptions) *streamingLayerReconciler {
	if opts.Resolver == nil || opts.Store == nil {
		return nil
	}

	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}

	if opts.Interval <= 0 {
		opts.Interval = 30 * time.Second
	}

	if opts.DispatchTimeout <= 0 {
		opts.DispatchTimeout = 30 * time.Second
	}

	return &streamingLayerReconciler{
		resolver:        opts.Resolver,
		store:           opts.Store,
		seeder:          opts.Seeder,
		logger:          opts.Logger.With(slog.String("subsystem", "streaming-layer-reconcile")),
		interval:        opts.Interval,
		dispatchTimeout: opts.DispatchTimeout,
		active:          map[digest.Digest]struct{}{},
	}
}

func (r *streamingLayerReconciler) Start(baseCtx context.Context, registry, repository string, manifestDigest digest.Digest, children []coldstart.ChildDigest) {
	if r == nil || len(children) == 0 || !r.claim(manifestDigest) {
		return
	}

	children = append([]coldstart.ChildDigest(nil), children...)

	go func() {
		defer r.release(manifestDigest)

		ctx := baseCtx

		r.logger.Info("streaming layer reconciliation started",
			slog.String("manifest", manifestDigest.String()),
			slog.Int("layers", len(children)),
		)

		for {
			pending := r.pending(ctx, children)
			if len(pending) == 0 {
				r.logger.Info("streaming layer reconciliation completed",
					slog.String("manifest", manifestDigest.String()),
				)

				return
			}

			if r.seeder != nil {
				go r.seeder.Seed(ctx, registry, repository, pending)
			}

			dispatchCtx, dispatchCancel := context.WithTimeout(ctx, r.dispatchTimeout)
			err := r.resolver.PrefetchStreamingManifestChildren(dispatchCtx, manifestDigest, pending, registry, repository)

			dispatchCancel()

			if err != nil && ctx.Err() == nil {
				r.logger.Debug("streaming layer reconciliation dispatch failed",
					slog.String("manifest", manifestDigest.String()),
					slog.Int("pending", len(pending)),
					slog.Any("err", err),
				)
			}

			timer := time.NewTimer(r.interval)
			select {
			case <-ctx.Done():
				timer.Stop()
				r.logger.Warn("streaming layer reconciliation stopped before convergence",
					slog.String("manifest", manifestDigest.String()),
					slog.Int("pending", len(pending)),
					slog.Any("err", context.Cause(ctx)),
				)

				return
			case <-timer.C:
			}
		}
	}()
}

func (r *streamingLayerReconciler) pending(ctx context.Context, children []coldstart.ChildDigest) []coldstart.ChildDigest {
	pending := make([]coldstart.ChildDigest, 0, len(children))

	for _, child := range children {
		checkCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		has, err := r.store.Has(checkCtx, child.Digest)

		cancel()

		if err != nil || !has {
			pending = append(pending, child)
		}
	}

	return pending
}

func (r *streamingLayerReconciler) claim(manifestDigest digest.Digest) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.active[manifestDigest]; ok {
		return false
	}

	r.active[manifestDigest] = struct{}{}

	return true
}

func (r *streamingLayerReconciler) release(manifestDigest digest.Digest) {
	r.mu.Lock()
	defer r.mu.Unlock()

	delete(r.active, manifestDigest)
}
