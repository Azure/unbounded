// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"flag"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"strconv"
	"strings"
	"time"
)

type config struct {
	showVersion                  bool
	endpoint, listen             string
	footprint, objectSize, seed  int64
	exponent                     float64
	concurrency, pageConcurrency int
	timeout, ttl, duration       time.Duration
}

func parseConfig(args []string, output io.Writer) (config, error) {
	var (
		c                     config
		footprint, objectSize string
	)

	f := flag.NewFlagSet("racer-loadgen", flag.ContinueOnError)
	f.SetOutput(output)
	f.BoolVar(&c.showVersion, "version", false, "print version and exit")
	f.StringVar(&c.endpoint, "endpoint", "http://racer-loadgen-volume.racer-system.svc", "Racer volume endpoint")
	f.StringVar(&c.listen, "listen", ":8080", "origin, /metrics and /healthz listen address")
	f.StringVar(&footprint, "footprint", "512GB", "shared logical dataset size (bytes, KB/MB/GB/TB, KiB/MiB/GiB/TiB)")
	f.StringVar(&objectSize, "object-size", "1GB", "fixed object size; must divide footprint exactly")
	f.Float64Var(&c.exponent, "exponent", 1, "Zipf exponent, finite and >= 0 (0 is uniform)")
	c.seed = rand.Int64()

	f.Func("seed", "sampling seed (int64; random by default)", func(s string) error {
		var err error

		c.seed, err = strconv.ParseInt(s, 10, 64)

		return err
	})
	f.IntVar(&c.concurrency, "concurrency", 4, "simultaneous full-object downloads")
	f.IntVar(&c.pageConcurrency, "page-concurrency", 8, "parallel page requests per object")
	f.DurationVar(&c.timeout, "timeout", 5*time.Minute, "deadline for each full-object download")
	f.DurationVar(&c.ttl, "ttl", time.Hour, "origin metadata TTL")
	f.DurationVar(&c.duration, "duration", 0, "run duration (0 runs until interrupted)")

	if err := f.Parse(args); err != nil {
		return c, err
	}

	if f.NArg() != 0 {
		return c, fmt.Errorf("unexpected arguments: %v", f.Args())
	}

	if c.showVersion {
		return c, nil
	}

	var err error
	if c.footprint, err = parseSize(footprint); err != nil {
		return c, fmt.Errorf("footprint: %w", err)
	}

	if c.objectSize, err = parseSize(objectSize); err != nil {
		return c, fmt.Errorf("object-size: %w", err)
	}

	if c.footprint%c.objectSize != 0 {
		return c, fmt.Errorf("footprint must be an exact multiple of object-size")
	}

	if c.footprint/c.objectSize > 1_000_000 {
		return c, fmt.Errorf("dataset exceeds 1000000 objects (sampler memory limit)")
	}

	if math.IsNaN(c.exponent) || math.IsInf(c.exponent, 0) || c.exponent < 0 {
		return c, fmt.Errorf("exponent must be finite and nonnegative")
	}

	if c.concurrency < 1 || c.pageConcurrency < 1 {
		return c, fmt.Errorf("concurrency and page-concurrency must be positive")
	}

	if c.timeout <= 0 || c.ttl < 0 || c.duration < 0 {
		return c, fmt.Errorf("timeout must be positive; ttl and duration must be nonnegative")
	}

	return c, nil
}

func parseSize(s string) (int64, error) {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}

	n, err := strconv.ParseInt(s[:i], 10, 64)
	units := map[string]int64{
		"": 1, "B": 1, "KB": 1000, "MB": 1000_000, "GB": 1000_000_000, "TB": 1000_000_000_000,
		"KIB": 1 << 10, "MIB": 1 << 20, "GIB": 1 << 30, "TIB": 1 << 40,
	}

	multiplier, ok := units[strings.ToUpper(s[i:])]
	if err != nil || !ok || n <= 0 || n > math.MaxInt64/multiplier {
		return 0, fmt.Errorf("invalid size %q: use a positive integer with an optional byte unit", s)
	}

	return n * multiplier, nil
}
