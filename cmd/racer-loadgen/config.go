// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"flag"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net/url"
	"strconv"
	"strings"
	"time"

	racermeta "github.com/Azure/unbounded/internal/racer"
)

type config struct {
	mode, role, registryListen, registryURL, registryNamespace, gantryEndpoint string
	layersPerImage, layerConcurrency                                           int
	showVersion                                                                bool
	endpoint, originSocket, listen                                             string
	footprint, objectSize, seed                                                int64
	exponent                                                                   float64
	concurrency, pageConcurrency                                               int
	timeout, ttl, duration                                                     time.Duration
	gantryReadyTimeout                                                         time.Duration
}

func parseConfig(args []string, output io.Writer) (config, error) {
	var (
		c                     config
		footprint, objectSize string
		cacheName             string
	)

	f := flag.NewFlagSet("racer-loadgen", flag.ContinueOnError)
	f.SetOutput(output)
	f.BoolVar(&c.showVersion, "version", false, "print version and exit")
	f.StringVar(&c.mode, "mode", "racer", "workload: racer or container-image")
	f.StringVar(&c.role, "role", "", "container-image role: registry, load, or both")
	f.StringVar(&c.registryListen, "registry-listen", ":8081", "fake registry TCP listen address")
	f.StringVar(&c.registryURL, "registry-url", "", "fake registry HTTP(S) URL for catalog discovery")
	f.StringVar(&c.registryNamespace, "registry-namespace", "", "Gantry upstream registry name (ns query parameter)")
	f.StringVar(&c.gantryEndpoint, "gantry-endpoint", "http://127.0.0.1:5000", "Gantry mirror HTTP(S) URL")
	f.DurationVar(&c.gantryReadyTimeout, "gantry-ready-timeout", 10*time.Minute, "both role: maximum wait for Gantry startup readiness after registry preparation")
	f.IntVar(&c.layersPerImage, "layers-per-image", 4, "unique layers per synthetic image; object-size is layer size")
	f.IntVar(&c.layerConcurrency, "layer-concurrency", 3, "parallel layer downloads per image")
	f.StringVar(&cacheName, "cache-name", "", "ClusterCache metadata.name deriving /run/racer/<name>/{client,origin}/socket")
	f.StringVar(&c.endpoint, "endpoint", "", "local Racer client Unix socket (ClusterCache.status.clientSocket)")
	f.StringVar(&c.originSocket, "origin-socket", "", "local origin Unix socket (ClusterCache.status.originSocket)")
	f.StringVar(&c.listen, "listen", ":8080", "management TCP address for /metrics and /healthz")
	f.StringVar(&footprint, "footprint", "512GB", "shared logical dataset size (bytes, KB/MB/GB/TB, KiB/MiB/GiB/TiB)")
	f.StringVar(&objectSize, "object-size", "1GB", "fixed object size; must divide footprint exactly")
	f.Float64Var(&c.exponent, "exponent", 1, "Zipf exponent, finite and >= 0 (0 is uniform)")
	c.seed = rand.Int64()

	f.Func("seed", "sampling seed (int64; random by default)", func(s string) error {
		var err error

		c.seed, err = strconv.ParseInt(s, 10, 64)

		return err
	})
	f.IntVar(&c.concurrency, "concurrency", 4, "simultaneous object downloads or image pulls")
	f.IntVar(&c.pageConcurrency, "page-concurrency", 8, "parallel page requests per object")
	f.DurationVar(&c.timeout, "timeout", 5*time.Minute, "deadline for each object download, image pull, or catalog request")
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

	if c.mode == "racer" {
		if cacheName != "" {
			if c.endpoint != "" || c.originSocket != "" {
				return c, fmt.Errorf("cache-name cannot be combined with endpoint or origin-socket")
			}

			var err error

			c.endpoint, c.originSocket, err = racermeta.CacheSockets(racermeta.SocketRoot, cacheName)
			if err != nil {
				return c, fmt.Errorf("cache-name: %w", err)
			}
		}

		if c.endpoint == "" || c.originSocket == "" {
			return c, fmt.Errorf("racer mode requires cache-name or both endpoint and origin-socket")
		}
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

	return c, c.validateMode()
}

func (c config) validateMode() error {
	switch c.mode {
	case "racer":
		if c.role != "" {
			return fmt.Errorf("role is only supported in container-image mode")
		}
	case "container-image":
		if c.role != "registry" && c.role != "load" && c.role != "both" {
			return fmt.Errorf("container-image mode requires -role=registry, -role=load, or -role=both")
		}

		if c.layersPerImage < 1 || c.layersPerImage > 1024 || c.layerConcurrency < 1 {
			return fmt.Errorf("layers-per-image must be 1..1024 and layer-concurrency must be positive")
		}

		if c.role != "load" {
			layers := c.footprint / c.objectSize
			if layers%int64(c.layersPerImage) != 0 || layers/int64(c.layersPerImage) > 100_000 {
				return fmt.Errorf("layer count must be divisible by layers-per-image and produce at most 100000 images")
			}
		}

		if c.role == "load" {
			if err := validateHTTPURL(c.registryURL); err != nil {
				return fmt.Errorf("registry-url: %w", err)
			}
		}

		if c.role == "both" && (c.gantryReadyTimeout <= 0 || c.registryURL != "") {
			return fmt.Errorf("both role requires positive gantry-ready-timeout and no registry-url (catalog is local)")
		}

		if c.role != "registry" {
			if err := validateHTTPURL(c.gantryEndpoint); err != nil {
				return fmt.Errorf("gantry-endpoint: %w", err)
			}

			if c.registryNamespace == "" || strings.ContainsAny(c.registryNamespace, "/?#@ \t\r\n") {
				return fmt.Errorf("registry-namespace must be a registry name, optionally with a port")
			}
		}
	default:
		return fmt.Errorf("unknown mode %q", c.mode)
	}

	return nil
}

func validateHTTPURL(value string) error {
	u, err := url.Parse(value)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return fmt.Errorf("expected an HTTP(S) origin URL without credentials, path, query, or fragment")
	}

	return nil
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
