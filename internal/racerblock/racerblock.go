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

// deleteDevice is ublk.Delete; tests replace it to observe replacement.
var deleteDevice = ublk.Delete

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

	// Validate before ReplaceExisting deletes a working device, so a config
	// that Start would reject cannot destroy the device it meant to replace.
	if err := cfg.Device.Validate(); err != nil {
		return fmt.Errorf("racerblock: invalid device config: %w", err)
	}

	if cfg.ReplaceExisting && cfg.Device.ID >= 0 {
		if err := deleteDevice(cfg.Device.ID); err != nil && !errors.Is(err, syscall.ENODEV) {
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

	runErr := waitAndClose(ctx, log, dev)

	stats := b.Stats()
	log.Info("device stopped", "path", dev.Path(), "stats", fmt.Sprintf("%+v", stats))

	return runErr
}

// runningDevice is the part of *ublk.Device that waitAndClose uses.
type runningDevice interface {
	Path() string
	Done() <-chan struct{}
	Err() error
	Close() error
}

// waitAndClose waits until ctx is canceled or dev stops, then closes dev.
// The device stops itself with ctx.Err() when ctx is canceled, so ctx and
// dev can both be done by the time this runs. The stop cause is read after
// Close, which waits for any teardown already in progress, so a failure
// that races with cancellation is not lost. Only a cause that is exactly
// ctx.Err() counts as a clean stop. Any other cause, including one that
// joins ctx.Err() with another error, is returned ahead of a Close error.
func waitAndClose(ctx context.Context, log *slog.Logger, dev runningDevice) error {
	select {
	case <-ctx.Done():
	case <-dev.Done():
	}

	// A device that stopped on its own before Close with no cause and no
	// cancellation is unexpected. A nil cause after our own Close is not.
	var stoppedFirst bool

	select {
	case <-dev.Done():
		stoppedFirst = true
	default:
	}

	log.Info("stopping device", "path", dev.Path())

	closeErr := dev.Close()

	ctxErr := ctx.Err()
	runErr := dev.Err()

	switch {
	case ctxErr != nil && runErr == ctxErr: //nolint:errorlint // The device records ctx.Err() itself; a wrapped or joined error is a different cause.
		runErr = nil
	case runErr == nil && stoppedFirst && ctxErr == nil:
		runErr = errors.New("racerblock: device stopped unexpectedly")
	}

	if runErr != nil {
		log.Error("device stopped", "path", dev.Path(), "err", runErr)
	}

	if closeErr != nil && runErr == nil {
		runErr = fmt.Errorf("racerblock: stop device: %w", closeErr)
	}

	return runErr
}

func closeBackend(log *slog.Logger, b *backend.Backend) {
	if err := b.Close(); err != nil {
		log.Warn("close backend", "err", err)
	}
}
