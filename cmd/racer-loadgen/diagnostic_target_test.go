// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/signal"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Build with go test -c and select only TestRacerDiagnosticTarget. This harness
// opens no listeners and never starts an origin, SDK, or dataplane process.
// Durations in the explicit JSON config are strings, for example "30s".
type diagnosticTargetConfig struct {
	Target, Namespace                         string
	Image                                     imageOptions
	ImageIndices                              []int
	Iterations, LayerConcurrency              int
	StartupTimeout, PullTimeout, TotalTimeout string
}

func runDiagnosticTarget(ctx context.Context, cfg diagnosticTargetConfig) error {
	startup, err := time.ParseDuration(cfg.StartupTimeout)
	if err != nil || startup <= 0 {
		return errors.New("positive startup timeout required")
	}

	pull, err := time.ParseDuration(cfg.PullTimeout)
	if err != nil || pull <= 0 {
		return errors.New("positive pull timeout required")
	}

	total, err := time.ParseDuration(cfg.TotalTimeout)
	if err != nil || total <= 0 {
		return errors.New("positive total timeout required")
	}

	if cfg.Iterations < 1 || cfg.LayerConcurrency < 1 || len(cfg.ImageIndices) == 0 || len(cfg.ImageIndices) > maxCatalogImages {
		return errors.New("positive iterations, layer concurrency, and selected images required")
	}

	for _, index := range cfg.ImageIndices {
		if index < 0 || index >= maxCatalogImages {
			return errors.New("image index out of range")
		}
	}

	ctx, cancel := context.WithTimeout(ctx, total)
	defer cancel()

	opts := pullOptions{
		Target: cfg.Target, Namespace: cfg.Namespace, Concurrency: 1,
		LayerConcurrency: cfg.LayerConcurrency, Timeout: pull, RetryDelay: time.Second,
		Verify: true, DiagnoseIntegrity: true, ZipfExponent: defaultZipfExponent,
	}

	p, err := newPuller(&syntheticImage{}, opts, pullTestMetrics())
	if err != nil {
		return err
	}
	defer p.close()

	startupCtx, stop := context.WithTimeout(ctx, startup)
	defer stop()

	images := make([]*syntheticImage, 0, len(cfg.ImageIndices))
	for _, index := range cfg.ImageIndices {
		img, err := newImage(startupCtx, catalogImageOptions(cfg.Image, index))
		if err != nil {
			return err
		}

		images = append(images, img)
	}

	stop()

	p.img = images[0]
	if err := p.configureDiagnostics(catalogFromImages(images)); err != nil {
		return err
	}

	for attempt := range cfg.Iterations {
		if err := ctx.Err(); err != nil {
			return err
		}

		if err := p.pullImage(ctx, images[attempt%len(images)]); err != nil {
			return err
		}
	}

	return ctx.Err()
}

func TestRacerDiagnosticTarget(t *testing.T) {
	path := os.Getenv("RACER_DIAGNOSTIC_CONFIG")
	if path == "" {
		t.Skip("requires explicit RACER_DIAGNOSTIC_CONFIG; never contacts a live target by default")
	}

	f, err := os.Open(path)
	require.NoError(t, err)

	defer f.Close()

	decoder := json.NewDecoder(io.LimitReader(f, 65537))
	decoder.DisallowUnknownFields()

	var cfg diagnosticTargetConfig
	require.NoError(t, decoder.Decode(&cfg))

	var extra any
	require.ErrorIs(t, decoder.Decode(&extra), io.EOF)
	// Error strings can contain URLs; production failure logging emits safe evidence.
	ctx, stop := signal.NotifyContext(t.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if runDiagnosticTarget(ctx, cfg) != nil {
		t.Fatal("diagnostic target failed; see sanitized pull evidence")
	}

	t.Logf("completed %d verified image pulls", cfg.Iterations)
}
