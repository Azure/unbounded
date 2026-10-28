// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"fmt"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/Azure/unbounded/internal/racer/workload"
)

// gpuStagingUnit mirrors GPU_STAGING_UNIT in cmd/racer/src/config.rs.
const gpuStagingUnit = 2 << 20

// Constants mirrored from cmd/racer/src: MIB, MAX_BYTES, and
// MAX_ENTRIES in config.rs, PAGE_BYTES in model, DUPLICATE_BYTES in
// read/candidates.rs, and the storage record size checked in config.rs
// (a page, a 16-byte tag, and MAX_HEADER_BYTES from store.rs).
const (
	mib            = 1 << 20
	maxBytes       = 64 << 30
	maxEntries     = 1 << 20
	pageBytes      = 16 * mib
	duplicateBytes = 2*pageBytes + 16
	recordBytes    = pageBytes + 16 + 16384 + 4096
)

// The checks below mirror the startup parsing in
// cmd/racer/src/config.rs so that a value the dataplane would refuse
// fails the operator plan instead of rolling Pods into a crash loop. They
// cover per-key syntax and ranges, not cross-key or hardware policy.
var (
	// Enumerated keys and their accepted values.
	dataplaneEnumKeys = map[string][]string{
		"RACER_ENABLE_RDMA":       {"auto", "true", "false"},
		"RACER_ENABLE_GPU":        {"auto", "true", "false"},
		"RACER_REPLICA_SELECTION": {"primary", "spread"},
		"RACER_ADMISSION_MODE":    {"disabled", "second-sight"},
	}

	// Booleans parse with Rust's bool FromStr, which accepts only true and false.
	dataplaneBoolKeys = []string{
		"RACER_ALLOW_SMT", "RACER_OPAQUE_RELAY", "RACER_PEER_TCP_NODELAY",
		"RACER_MEMORY_PRESSURE", "RACER_PPROF_ENABLED", "RACER_GPU_PCI_P2PDMA",
	}

	// Inclusive single-key ranges from Config::from_lookup and the validate
	// methods it calls. Cross-key rules, such as RACER_CRYPTO_HELPERS at most
	// RACER_IO_WORKERS or plaintext budgets that must fit the range window,
	// are left to the dataplane.
	dataplaneRangeKeys = map[string][2]uint64{
		"RACER_GPU_LANDING_BYTES":            {0, math.MaxUint64},
		"RACER_IO_WORKERS":                   {1, 65536},
		"RACER_CRYPTO_HELPERS":               {1, 65536},
		"RACER_SHARES":                       {1, math.MaxUint32},
		"RACER_DISK_PAGE_ENTRIES":            {1, maxEntries},
		"RACER_ADMISSION_HISTORY_BYTES":      {32, 512 * mib},
		"RACER_ADMISSION_PERIOD_SECS":        {1, 86400},
		"RACER_CHECKPOINT_BYTES":             {1, 512 * mib},
		"RACER_DEVICE_BUSY_WAIT_SECS":        {0, 3600},
		"RACER_WARM_MAX_TTL_SECS":            {1, 86400},
		"RACER_WARM_FILL_TIMEOUT_SECS":       {1, 86400},
		"RACER_WARM_MAX_JOBS":                {1, 1024},
		"RACER_WARM_WINDOW_PAGES":            {1, 64},
		"RACER_RETAINED_SEGMENT_PERCENT":     {1, 100},
		"RACER_RANGE_SPILL_PAGES":            {0, 1024},
		"RACER_PAGE_HEDGE_DELAY_MS":          {1, 30000},
		"RACER_PAGE_HEDGE_SLOTS":             {0, 32},
		"RACER_PAGE_HEDGE_BYTES":             {0, 32 * duplicateBytes},
		"RACER_PEER_INFLIGHT_MAX":            {1, 65536},
		"RACER_PEER_PER_NEIGHBOR_MAX":        {1, 65536},
		"RACER_PEER_RECEIVE_MAX":             {0, 65536},
		"RACER_PEER_RECEIVE_QUEUE_MAX":       {1, 65536},
		"RACER_PEER_RECEIVE_WAIT_MS":         {1, 30000},
		"RACER_SLAB_BYTES":                   {1, math.MaxInt64},
		"RACER_SEGMENT_BYTES":                {recordBytes, math.MaxInt64},
		"RACER_FREE_SEGMENT_RESERVE":         {1, maxEntries - 1},
		"RACER_PLAINTEXT_BYTES":              {1, maxBytes},
		"RACER_CIPHERTEXT_BYTES":             {1, maxBytes},
		"RACER_DIRTY_BYTES":                  {pageBytes + 16, maxBytes},
		"RACER_REGISTERED_BYTES":             {1, maxBytes},
		"RACER_REQUEST_CONTEXT_BYTES":        {128 * 1024, maxBytes},
		"RACER_FLIGHTS":                      {1, 65536},
		"RACER_WAITERS_PER_FLIGHT":           {1, 4096},
		"RACER_QUEUE_ENTRIES":                {2, 65536},
		"RACER_CONNECTIONS_PER_NEIGHBOR":     {1, 1024},
		"RACER_CLIENT_CONNECTIONS":           {1, 65536},
		"RACER_PIPES":                        {1, 65536},
		"RACER_RANGE_WINDOW_PAGES":           {1, 64},
		"RACER_HEADER_BYTES":                 {1024, 32 * 1024},
		"RACER_PLACEMENT_CACHE_BYTES":        {1024, 512 * mib},
		"RACER_CACHED_PATHS":                 {1, maxEntries},
		"RACER_PATH_CACHE_BYTES":             {1, 512 * mib},
		"RACER_ACTIVE_PATH_SEARCHES":         {1, 64},
		"RACER_RETAINED_SNAPSHOTS":           {2, 64},
		"RACER_METADATA_ENTRIES":             {1, maxEntries},
		"RACER_RELAY_TRANSFERS":              {1, 65536},
		"RACER_ORIGIN_CONNECTIONS_PER_CACHE": {1, 1024},
		"RACER_REQUEST_TIMEOUT_MS":           {1, 86400000},
		"RACER_PEER_ATTEMPT_TIMEOUT_MS":      {1, 86400000},
		"RACER_READER_STALL_TIMEOUT_MS":      {1, 86400000},
		"RACER_SHUTDOWN_TIMEOUT_MS":          {1, 3600000},
	}
)

// validateDataplaneConfig reports every racer-settings value the
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
