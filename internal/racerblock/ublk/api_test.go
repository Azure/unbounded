// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package ublk

import (
	"context"
	"errors"
	"math"
	"runtime"
	"strconv"
	"syscall"
	"testing"
)

func TestConfigDefaults(t *testing.T) {
	cfg, err := Config{ID: -1, Size: 4096}.withDefaults()
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Queues != min(runtime.NumCPU(), 4) || cfg.Depth != 128 || cfg.MaxIOBytes != 1<<20 || cfg.Logger == nil {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
}

// largeDeviceIDs returns out-of-range IDs, including ones that truncate to a
// valid uint32 ID on 64-bit platforms.
func largeDeviceIDs() []int {
	ids := []int{-2, MaxDeviceID + 1, math.MaxInt32, math.MinInt}

	if strconv.IntSize == 64 {
		// Computed at run time so the file still compiles where int is 32 bits.
		shift := 32
		ids = append(ids, 1<<shift, 1<<shift+1, math.MaxInt)
	}

	return ids
}

// TestValidateDeviceIDKernelBound pins the kernel limit with literals so the
// test does not depend on MaxDeviceID itself: the kernel rejects
// id >= UBLK_MAX_UBLKS (1<<20).
func TestValidateDeviceIDKernelBound(t *testing.T) {
	if err := validateDeviceID(1<<20 - 1); err != nil {
		t.Errorf("validateDeviceID(1<<20-1) = %v, want nil", err)
	}

	if err := validateDeviceID(1 << 20); err == nil {
		t.Error("validateDeviceID(1<<20) = nil, want error")
	}

	if err := (Config{ID: 1 << 20, Size: 512}).Validate(); err == nil {
		t.Error("Config{ID: 1<<20}.Validate() = nil, want error")
	}
}

func TestValidateDeviceID(t *testing.T) {
	for _, id := range []int{0, 1, MaxDeviceID} {
		if err := validateDeviceID(id); err != nil {
			t.Errorf("validateDeviceID(%d) = %v", id, err)
		}

		if _, err := (Config{ID: id, Size: 512}).withDefaults(); err != nil {
			t.Errorf("Config{ID: %d} = %v", id, err)
		}
	}

	// -1 asks the kernel to choose, so Config accepts it but Delete does not.
	if err := validateDeviceID(-1); err == nil {
		t.Error("validateDeviceID(-1) succeeded")
	}

	if _, err := (Config{ID: -1, Size: 512}).withDefaults(); err != nil {
		t.Errorf("Config{ID: -1} = %v", err)
	}

	for _, id := range largeDeviceIDs() {
		if err := validateDeviceID(id); err == nil {
			t.Errorf("validateDeviceID(%d) succeeded", id)
		}

		if _, err := (Config{ID: id, Size: 512}).withDefaults(); err == nil {
			t.Errorf("Config{ID: %d} succeeded", id)
		}
	}
}

func TestConfigValidation(t *testing.T) {
	ok := Config{ID: -1, Size: 512}
	cases := map[string]func(c *Config){
		"zero size":        func(c *Config) { c.Size = 0 },
		"negative size":    func(c *Config) { c.Size = -512 },
		"unaligned size":   func(c *Config) { c.Size = 1000 },
		"bad id":           func(c *Config) { c.ID = -2 },
		"id above max":     func(c *Config) { c.ID = MaxDeviceID + 1 },
		"negative queues":  func(c *Config) { c.Queues = -1 },
		"too many queues":  func(c *Config) { c.Queues = MaxQueues + 1 },
		"negative depth":   func(c *Config) { c.Depth = -1 },
		"too deep":         func(c *Config) { c.Depth = MaxQueueDepth + 1 },
		"unaligned max io": func(c *Config) { c.MaxIOBytes = 4097 },
		"negative max io":  func(c *Config) { c.MaxIOBytes = -4096 },
		"huge max io":      func(c *Config) { c.MaxIOBytes = 64 << 20 },
	}

	for name, mut := range cases {
		c := ok
		mut(&c)

		if _, err := c.withDefaults(); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}

	edge := Config{ID: 7, Size: 512, Queues: MaxQueues, Depth: MaxQueueDepth, MaxIOBytes: 4096}
	if got, err := edge.withDefaults(); err != nil || got.ID != 7 || got.Queues != MaxQueues {
		t.Fatalf("edge config rejected: %v %+v", err, got)
	}
}

func TestDispatch(t *testing.T) {
	cases := []struct {
		op     uint32
		action dispatchAction
		res    int32
	}{
		{opRead, actionRead, 0},
		{opRead | 1<<13, actionRead, 0}, // flags above the op byte are ignored
		{opWrite, actionComplete, -int32(syscall.EROFS)},
		{opFlush, actionComplete, 0},
		{opDiscard, actionComplete, -int32(syscall.EOPNOTSUPP)},
		{opWriteSame, actionComplete, -int32(syscall.EOPNOTSUPP)},
		{opWriteZeroes, actionComplete, -int32(syscall.EOPNOTSUPP)},
		{13, actionComplete, -int32(syscall.EOPNOTSUPP)},
		{0xff, actionComplete, -int32(syscall.EOPNOTSUPP)},
	}
	for _, c := range cases {
		a, r := dispatch(c.op)
		if a != c.action || r != c.res {
			t.Errorf("dispatch(%#x) = %v, %d; want %v, %d", c.op, a, r, c.action, c.res)
		}
	}
}

type funcHandler func(ctx context.Context, p []byte, off int64) error

func (f funcHandler) ReadAt(ctx context.Context, p []byte, off int64) error { return f(ctx, p, off) }

func TestServeRead(t *testing.T) {
	buf := make([]byte, 4096)

	res, err := serveRead(context.Background(), funcHandler(func(_ context.Context, p []byte, off int64) error {
		if off != 8192 {
			t.Errorf("off = %d", off)
		}

		p[0] = 1

		return nil
	}), buf, 8192)
	if err != nil || res != 4096 || buf[0] != 1 {
		t.Fatalf("success: res=%d err=%v", res, err)
	}

	res, _ = serveRead(context.Background(), funcHandler(func(context.Context, []byte, int64) error {
		return syscall.ENOSPC
	}), buf, 0)
	if res != -int32(syscall.ENOSPC) {
		t.Fatalf("errno passthrough: %d", res)
	}

	res, _ = serveRead(context.Background(), funcHandler(func(context.Context, []byte, int64) error {
		return errors.New("boom")
	}), buf, 0)
	if res != -int32(syscall.EIO) {
		t.Fatalf("generic error: %d", res)
	}

	res, err = serveRead(context.Background(), funcHandler(func(context.Context, []byte, int64) error {
		panic("oops")
	}), buf, 0)
	if res != -int32(syscall.EIO) || err == nil {
		t.Fatalf("panic: %d %v", res, err)
	}

	if r := errnoResult(syscall.Errno(0)); r != -int32(syscall.EIO) {
		t.Fatalf("zero errno: %d", r)
	}
}

// TestConfigValidateMatchesWithDefaults checks that Validate rejects the
// configs Start rejects and accepts valid ones.
func TestConfigValidateMatchesWithDefaults(t *testing.T) {
	if err := (Config{ID: 7, Size: 512, MaxIOBytes: 4097}).Validate(); err == nil {
		t.Fatal("Validate accepted unaligned MaxIOBytes")
	}

	if err := (Config{ID: MaxDeviceID + 1, Size: 512}).Validate(); err == nil {
		t.Fatal("Validate accepted out-of-range ID")
	}

	if err := (Config{ID: 7, Size: 512}).Validate(); err != nil {
		t.Fatalf("Validate rejected valid config: %v", err)
	}
}
