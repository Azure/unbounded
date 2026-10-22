// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package racerblock serves one Racer object as a read-only Linux ublk block
// device. It joins the chunk cache in package backend with the ublk server in
// package ublk and owns their life cycle.
package racerblock

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"syscall"

	"github.com/Azure/unbounded/internal/racerblock/backend"
	"github.com/Azure/unbounded/internal/racerblock/ublk"
)

// SectorSize is the logical block size of every racer-block device.
const SectorSize = 512

// Config describes one racer-block device.
type Config struct {
	// Source reads the Racer object. Required.
	Source backend.Source
	// Backend configures the chunk cache, readahead, and ETag pinning.
	Backend backend.Config
	// Device configures the ublk device. Size is ignored and set from the
	// object size rounded up to SectorSize.
	Device ublk.Config
	// ReplaceExisting deletes a device left behind with the same Device.ID
	// before starting. It has no effect when Device.ID is negative.
	ReplaceExisting bool
	// Ready is called once with the block device path after the device starts.
	Ready func(path string)
	// Logger receives life cycle events. Nil uses slog.Default.
	Logger *slog.Logger
}

// DeviceSize returns the block device size for an object of objectSize bytes.
// The size is rounded up to a whole sector; the backend fills the tail with
// zeros.
func DeviceSize(objectSize int64) (int64, error) {
	if objectSize <= 0 {
		return 0, fmt.Errorf("racerblock: object is empty (size %d)", objectSize)
	}

	if objectSize > (1<<63-1)-(SectorSize-1) {
		return 0, fmt.Errorf("racerblock: object size %d is too large", objectSize)
	}

	return (objectSize + SectorSize - 1) / SectorSize * SectorSize, nil
}

// Run opens the object, starts the device, and serves it until ctx is
// canceled or the device stops. It stops and deletes the device before it
// returns. Cancellation of ctx is a clean stop and returns nil.
func Run(ctx context.Context, cfg Config) error {
	if cfg.Source == nil {
		return errors.New("racerblock: Source is required")
	}

	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}

	if cfg.Backend.Logger == nil {
		cfg.Backend.Logger = log.With("component", "backend")
	}

	if cfg.Device.Logger == nil {
		cfg.Device.Logger = log.With("component", "ublk")
	}

	b, err := backend.Open(ctx, cfg.Source, cfg.Backend)
	if err != nil {
		return fmt.Errorf("racerblock: open object: %w", err)
	}
	defer closeBackend(log, b)

	size, err := DeviceSize(b.Size())
	if err != nil {
		return err
	}

	cfg.Device.Size = size

	if cfg.ReplaceExisting && cfg.Device.ID >= 0 {
		if err := ublk.Delete(cfg.Device.ID); err != nil && !errors.Is(err, syscall.ENODEV) {
			return fmt.Errorf("racerblock: delete existing device %d: %w", cfg.Device.ID, err)
		}
	}

	dev, err := ublk.Start(ctx, cfg.Device, b)
	if err != nil {
		return fmt.Errorf("racerblock: start device: %w", err)
	}

	log.Info("device ready", "path", dev.Path(), "id", dev.ID(),
		"objectBytes", b.Size(), "deviceBytes", size, "etag", b.ETag())

	if cfg.Ready != nil {
		cfg.Ready(dev.Path())
	}

	var runErr error

	select {
	case <-ctx.Done():
		log.Info("stopping device", "path", dev.Path())
	case <-dev.Done():
		runErr = dev.Err()
		if runErr == nil {
			runErr = errors.New("racerblock: device stopped unexpectedly")
		}

		log.Error("device stopped", "path", dev.Path(), "err", runErr)
	}

	if err := dev.Close(); err != nil && runErr == nil {
		runErr = fmt.Errorf("racerblock: stop device: %w", err)
	}

	stats := b.Stats()
	log.Info("device stopped", "path", dev.Path(), "stats", fmt.Sprintf("%+v", stats))

	return runErr
}

func closeBackend(log *slog.Logger, b *backend.Backend) {
	if err := b.Close(); err != nil {
		log.Warn("close backend", "err", err)
	}
}
