// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package ublk

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
)

// ErrUnsupported is returned when ublk is not available on this platform.
var ErrUnsupported = errors.New("ublk: not supported on this platform")

const (
	// MaxQueueDepth is UBLK_MAX_QUEUE_DEPTH.
	MaxQueueDepth = 4096
	// MaxQueues is UBLK_MAX_NR_QUEUES.
	MaxQueues = 4096

	// MaxDeviceID is the largest device ID that Start and Delete accept.
	MaxDeviceID = 1 << 20

	// DefaultDepth is the per-queue depth used when Config.Depth is 0.
	DefaultDepth = 128

	defaultMaxQueues  = 4
	defaultMaxIOBytes = 1 << 20
	sectorSize        = 512
	ioAlign           = 4096
	// maxIOBytesLimit bounds MaxIOBytes to what the kernel driver accepts
	// (UBLK_IO_BUF_BITS = 25, i.e. 32 MiB).
	maxIOBytesLimit = 1 << 25
)

// DefaultQueues returns the queue count used when Config.Queues is 0.
func DefaultQueues() int {
	return min(runtime.NumCPU(), defaultMaxQueues)
}

// Handler serves device reads. ReadAt must fill all of p with device bytes
// starting at off. It may block. It is called concurrently from worker
// goroutines, never from a queue thread. ctx is canceled when the device
// stops. A returned syscall.Errno is passed to the kernel as is; any other
// error becomes EIO.
type Handler interface {
	ReadAt(ctx context.Context, p []byte, off int64) error
}

// Config describes the device to create.
type Config struct {
	ID         int          // requested device id; -1 lets the kernel choose
	Size       int64        // bytes; >0 and a multiple of 512
	Queues     int          // 0 -> default min(runtime.NumCPU(), 4)
	Depth      int          // per-queue depth; 0 -> 128; max MaxQueueDepth
	MaxIOBytes int          // 0 -> 1 MiB; a multiple of 4096
	Logger     *slog.Logger // nil -> slog.Default()
}

// validateDeviceID reports an error unless id is a fixed device ID in
// [0, MaxDeviceID]. Callers check this before sending any control command,
// because those commands truncate the ID to 32 bits.
func validateDeviceID(id int) error {
	if id < 0 || id > MaxDeviceID {
		return fmt.Errorf("ublk: invalid device id %d", id)
	}

	return nil
}

// Validate reports whether Start would accept cfg, without touching the
// kernel. Callers use it to reject a bad config before destructive steps
// such as deleting an existing device.
func (cfg Config) Validate() error {
	_, err := cfg.withDefaults()

	return err
}

// withDefaults validates cfg and returns a copy with defaults applied.
func (cfg Config) withDefaults() (Config, error) {
	if cfg.ID != -1 {
		if err := validateDeviceID(cfg.ID); err != nil {
			return cfg, err
		}
	}

	if cfg.Size <= 0 || cfg.Size%sectorSize != 0 {
		return cfg, fmt.Errorf("ublk: size %d must be positive and a multiple of %d", cfg.Size, sectorSize)
	}

	if cfg.Queues == 0 {
		cfg.Queues = DefaultQueues()
	}

	if cfg.Queues < 0 || cfg.Queues > MaxQueues {
		return cfg, fmt.Errorf("ublk: queues %d out of range [1, %d]", cfg.Queues, MaxQueues)
	}

	if cfg.Depth == 0 {
		cfg.Depth = DefaultDepth
	}

	if cfg.Depth < 0 || cfg.Depth > MaxQueueDepth {
		return cfg, fmt.Errorf("ublk: depth %d out of range [1, %d]", cfg.Depth, MaxQueueDepth)
	}

	if cfg.MaxIOBytes == 0 {
		cfg.MaxIOBytes = defaultMaxIOBytes
	}

	if cfg.MaxIOBytes < 0 || cfg.MaxIOBytes%ioAlign != 0 || cfg.MaxIOBytes > maxIOBytesLimit {
		return cfg, fmt.Errorf("ublk: max io bytes %d must be a positive multiple of %d no larger than %d",
			cfg.MaxIOBytes, ioAlign, maxIOBytesLimit)
	}

	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}

	return cfg, nil
}
