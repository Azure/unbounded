// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/Azure/unbounded/internal/racer/workload"
)

// CUDAKey selects the CUDA dataplane image, read from racer-dataplane-config
// by the operator. "true" runs racer-dataplane-cuda; "false" or unset runs
// racer-dataplane.
const CUDAKey = "RACER_DATAPLANE_CUDA"

// cudaImageName is the dataplane image built with RACER_NATIVE_CUDA=true.
const cudaImageName = "racer-dataplane-cuda"

// gpuStagingUnit mirrors GPU_STAGING_UNIT in cmd/racer-dataplane/src/config.rs.
const gpuStagingUnit = 2 << 20

// The checks below mirror the startup parsing in
// cmd/racer-dataplane/src/config.rs so that a value the dataplane would refuse
// fails the operator plan instead of rolling Pods into a crash loop. They
// cover per-key syntax and ranges, not cross-key or hardware policy.
var (
	// Enumerated keys and their accepted values.
	dataplaneEnumKeys = map[string][]string{
		"RACER_ENABLE_RDMA":       {"auto", "true", "false"},
		"RACER_ENABLE_GPU":        {"auto", "true", "false"},
		"RACER_REPLICA_SELECTION": {"primary", "spread"},
		"RACER_ADMISSION_MODE":    {"disabled", "second-sight"},
		CUDAKey:                   {"true", "false"},
	}

	// Booleans parse with Rust's bool FromStr, which accepts only true and false.
	dataplaneBoolKeys = []string{
		"RACER_ALLOW_SMT", "RACER_OPAQUE_RELAY", "RACER_PEER_TCP_NODELAY",
		"RACER_MEMORY_PRESSURE", "RACER_PPROF_ENABLED",
	}

	// Unsigned decimal keys that may be zero.
	dataplaneDecimalKeys = []string{
		"RACER_GPU_LANDING_BYTES", "RACER_IO_WORKERS", "RACER_CRYPTO_HELPERS",
		"RACER_RANGE_SPILL_PAGES", "RACER_PAGE_HEDGE_DELAY_MS", "RACER_PAGE_HEDGE_SLOTS",
		"RACER_PAGE_HEDGE_BYTES", "RACER_PEER_INFLIGHT_MAX", "RACER_PEER_PER_NEIGHBOR_MAX",
		"RACER_PEER_RECEIVE_MAX", "RACER_PEER_RECEIVE_QUEUE_MAX", "RACER_PEER_RECEIVE_WAIT_MS",
		"RACER_SLAB_BYTES", "RACER_SEGMENT_BYTES", "RACER_FREE_SEGMENT_RESERVE",
		"RACER_REQUEST_TIMEOUT_MS", "RACER_PEER_ATTEMPT_TIMEOUT_MS", "RACER_READER_STALL_TIMEOUT_MS",
		"RACER_SHUTDOWN_TIMEOUT_MS", "RACER_ADMISSION_PERIOD_SECS", "RACER_DEVICE_BUSY_WAIT_SECS",
	}

	// Unsigned decimal keys that must be nonzero.
	dataplaneNonzeroKeys = []string{
		"RACER_SHARES", "RACER_DISK_PAGE_ENTRIES", "RACER_ADMISSION_HISTORY_BYTES",
		"RACER_CHECKPOINT_BYTES", "RACER_PLAINTEXT_BYTES", "RACER_CIPHERTEXT_BYTES",
		"RACER_DIRTY_BYTES", "RACER_REGISTERED_BYTES", "RACER_REQUEST_CONTEXT_BYTES",
		"RACER_FLIGHTS", "RACER_WAITERS_PER_FLIGHT", "RACER_QUEUE_ENTRIES",
		"RACER_CONNECTIONS_PER_NEIGHBOR", "RACER_CLIENT_CONNECTIONS", "RACER_PIPES",
		"RACER_RANGE_WINDOW_PAGES", "RACER_HEADER_BYTES", "RACER_PLACEMENT_CACHE_BYTES",
		"RACER_CACHED_PATHS", "RACER_PATH_CACHE_BYTES", "RACER_ACTIVE_PATH_SEARCHES",
		"RACER_RETAINED_SNAPSHOTS", "RACER_METADATA_ENTRIES", "RACER_RELAY_TRANSFERS",
		"RACER_ORIGIN_CONNECTIONS_PER_CACHE",
	}

	// Inclusive ranges enforced by the dataplane's warm read validation.
	dataplaneRangeKeys = map[string][2]uint64{
		"RACER_WARM_MAX_TTL_SECS":        {1, 86400},
		"RACER_WARM_FILL_TIMEOUT_SECS":   {1, 86400},
		"RACER_WARM_MAX_JOBS":            {1, 1024},
		"RACER_WARM_WINDOW_PAGES":        {1, 64},
		"RACER_RETAINED_SEGMENT_PERCENT": {1, 100},
	}
)

// validateDataplaneConfig reports every racer-dataplane-config value the
// dataplane or operator would reject, sorted by key, in one error.
func validateDataplaneConfig(data map[string]string) error {
	var problems []string

	add := func(key, rule string) {
		problems = append(problems, key+" "+rule)
	}

	for key, allowed := range dataplaneEnumKeys {
		if value, ok := data[key]; ok && !slices.Contains(allowed, value) {
			add(key, "must be one of "+strings.Join(allowed, ", "))
		}
	}

	for _, key := range dataplaneBoolKeys {
		if value, ok := data[key]; ok && value != "true" && value != "false" {
			add(key, "must be true or false")
		}
	}

	for _, key := range dataplaneDecimalKeys {
		if value, ok := data[key]; ok {
			if _, valid := dataplaneDecimal(value); !valid {
				add(key, "must be an unsigned decimal integer")
			}
		}
	}

	for _, key := range dataplaneNonzeroKeys {
		if value, ok := data[key]; ok {
			if n, valid := dataplaneDecimal(value); !valid || n == 0 {
				add(key, "must be a positive decimal integer")
			}
		}
	}

	for key, bounds := range dataplaneRangeKeys {
		if value, ok := data[key]; ok {
			if n, valid := dataplaneDecimal(value); !valid || n < bounds[0] || n > bounds[1] {
				add(key, fmt.Sprintf("must be an integer from %d to %d", bounds[0], bounds[1]))
			}
		}
	}

	if value, ok := data["RACER_GPU_STAGING_BYTES"]; ok {
		if n, valid := dataplaneDecimal(value); !valid || n < gpuStagingUnit || n%gpuStagingUnit != 0 {
			add("RACER_GPU_STAGING_BYTES", "must be a nonzero multiple of 2097152 (2 MiB)")
		}
	}

	if value, ok := data["RACER_GPU_LANDING_BYTES"]; ok {
		if n, valid := dataplaneDecimal(value); valid && n%gpuStagingUnit != 0 {
			add("RACER_GPU_LANDING_BYTES", "must be 0 or a multiple of 2097152 (2 MiB)")
		}
	}

	_, ioSet := data["RACER_IO_WORKERS"]
	if _, helpersSet := data["RACER_CRYPTO_HELPERS"]; ioSet != helpersSet {
		add("RACER_IO_WORKERS", "and RACER_CRYPTO_HELPERS must be set together")
	}

	if value, ok := data[workload.ControlSeatsKey]; ok && value != "auto" {
		// The runtime accepts only the canonical decimal form, so reject
		// leading zeros, signs, and whitespace before Pods roll.
		seats, err := strconv.ParseUint(value, 10, 8)
		if err != nil || strconv.FormatUint(seats, 10) != value || seats < 1 || seats > workload.MaxControlSeats {
			add(workload.ControlSeatsKey, fmt.Sprintf("must be auto or an integer from 1 to %d", workload.MaxControlSeats))
		}
	}

	if value, ok := data[workload.NVIDIAKey]; ok {
		if mode := workload.NVIDIAInjection(value); mode != workload.NVIDIAOff && mode != workload.NVIDIAEnv && mode != workload.NVIDIACDI {
			add(workload.NVIDIAKey, "must be off, env, or cdi")
		}
	}

	if value, ok := data["RACER_MAX_THREADS"]; ok {
		if threads, valid := dataplaneDecimal(value); !valid || threads < minThreads {
			add("RACER_MAX_THREADS", fmt.Sprintf("must be an integer of at least %d, including the control plane thread", minThreads))
		}
	}

	if len(problems) == 0 {
		return nil
	}

	sort.Strings(problems)

	return fmt.Errorf("ConfigMap %s is invalid, so the dataplane DaemonSet keeps its current configuration: %s", dataplaneConfigName, strings.Join(problems, "; "))
}

// dataplaneDecimal mirrors the dataplane's text and decimal rules: nonempty,
// at most 4096 bytes, no control characters, ASCII digits only, and within
// the u64 range.
func dataplaneDecimal(value string) (uint64, bool) {
	if value == "" || len(value) > 4096 || strings.IndexFunc(value, func(r rune) bool { return r < '0' || r > '9' || unicode.IsControl(r) }) >= 0 {
		return 0, false
	}

	trimmed := strings.TrimLeft(value, "0")
	if trimmed == "" {
		return 0, true
	}

	n, err := strconv.ParseUint(trimmed, 10, 64)

	return n, err == nil
}

// dataplaneImage returns the dataplane image ConfigMap data selects.
func dataplaneImage(imageFor func(string) string, data map[string]string) string {
	if data[CUDAKey] == "true" {
		return imageFor(cudaImageName)
	}

	return imageFor(dataplaneImageName)
}
