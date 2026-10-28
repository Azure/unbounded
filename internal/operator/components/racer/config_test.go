// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

func TestValidateDataplaneConfigAccepts(t *testing.T) {
	for name, data := range map[string]map[string]string{
		"empty": {},
		"gpu": {
			"RACER_ENABLE_GPU": "true", "RACER_GPU_STAGING_BYTES": "2097152",
			"RACER_GPU_LANDING_BYTES": "0", "RACER_NVIDIA": "cdi",
			"RACER_GPU_PCI_P2PDMA": "true",
		},
		"gpu pci p2pdma off":          {"RACER_GPU_PCI_P2PDMA": "false"},
		"gpu auto with large landing": {"RACER_ENABLE_GPU": "auto", "RACER_GPU_LANDING_BYTES": "268435456"},
		"booleans and enums": {
			"RACER_ALLOW_SMT": "true", "RACER_PPROF_ENABLED": "false", "RACER_ENABLE_RDMA": "false",
			"RACER_REPLICA_SELECTION": "primary", "RACER_ADMISSION_MODE": "disabled",
		},
		"leading zeros and bounds": {
			"RACER_MAX_THREADS": "0004", "RACER_WARM_MAX_JOBS": "1024", "RACER_RETAINED_SEGMENT_PERCENT": "1",
			"RACER_RANGE_SPILL_PAGES": "0", "RACER_SHARES": "4294967295",
		},
		"range minimums": {
			"RACER_IO_WORKERS": "1", "RACER_CRYPTO_HELPERS": "1", "RACER_SHARES": "1",
			"RACER_ADMISSION_HISTORY_BYTES": "32", "RACER_DEVICE_BUSY_WAIT_SECS": "0",
			"RACER_PAGE_HEDGE_SLOTS": "0", "RACER_PEER_RECEIVE_MAX": "0",
			"RACER_SLAB_BYTES": "1", "RACER_SEGMENT_BYTES": "16797712", "RACER_FREE_SEGMENT_RESERVE": "1",
			"RACER_DIRTY_BYTES": "16777232", "RACER_REQUEST_CONTEXT_BYTES": "131072",
			"RACER_QUEUE_ENTRIES": "2", "RACER_HEADER_BYTES": "1024", "RACER_PLACEMENT_CACHE_BYTES": "1024",
			"RACER_RETAINED_SNAPSHOTS": "2", "RACER_RANGE_WINDOW_PAGES": "1", "RACER_REQUEST_TIMEOUT_MS": "1",
		},
		"range maximums": {
			"RACER_IO_WORKERS": "65536", "RACER_CRYPTO_HELPERS": "65536", "RACER_DISK_PAGE_ENTRIES": "1048576",
			"RACER_ADMISSION_HISTORY_BYTES": "536870912", "RACER_ADMISSION_PERIOD_SECS": "86400",
			"RACER_CHECKPOINT_BYTES": "536870912", "RACER_DEVICE_BUSY_WAIT_SECS": "3600",
			"RACER_RANGE_SPILL_PAGES": "1024", "RACER_PAGE_HEDGE_DELAY_MS": "30000", "RACER_PAGE_HEDGE_SLOTS": "32",
			"RACER_PAGE_HEDGE_BYTES": "1073742336", "RACER_PEER_INFLIGHT_MAX": "65536",
			"RACER_PEER_RECEIVE_MAX": "65536", "RACER_PEER_RECEIVE_QUEUE_MAX": "65536",
			"RACER_PEER_RECEIVE_WAIT_MS": "30000", "RACER_SLAB_BYTES": "9223372036854775807",
			"RACER_FREE_SEGMENT_RESERVE": "1048575", "RACER_PLAINTEXT_BYTES": "68719476736",
			"RACER_FLIGHTS": "65536", "RACER_WAITERS_PER_FLIGHT": "4096", "RACER_QUEUE_ENTRIES": "65536",
			"RACER_CONNECTIONS_PER_NEIGHBOR": "1024", "RACER_PIPES": "65536", "RACER_RANGE_WINDOW_PAGES": "64",
			"RACER_HEADER_BYTES": "32768", "RACER_PLACEMENT_CACHE_BYTES": "536870912",
			"RACER_CACHED_PATHS": "1048576", "RACER_ACTIVE_PATH_SEARCHES": "64", "RACER_RETAINED_SNAPSHOTS": "64",
			"RACER_ORIGIN_CONNECTIONS_PER_CACHE": "1024", "RACER_REQUEST_TIMEOUT_MS": "86400000",
			"RACER_SHUTDOWN_TIMEOUT_MS": "3600000",
		},
		"thread split":             {"RACER_IO_WORKERS": "2", "RACER_CRYPTO_HELPERS": "1", "RACER_CONTROL_SEATS": "auto"},
		"unknown keys are ignored": {"RACER_SOMETHING_ELSE": "x"},
	} {
		t.Run(name, func(t *testing.T) {
			require.NoError(t, validateDataplaneConfig(data))
		})
	}
}

func TestValidateDataplaneConfigRejects(t *testing.T) {
	for _, tc := range []struct{ key, value string }{
		{"RACER_ENABLE_GPU", "TRUE"},
		{"RACER_ENABLE_GPU", " auto"},
		{"RACER_ENABLE_GPU", ""},
		{"RACER_ENABLE_RDMA", "yes"},
		{"RACER_GPU_STAGING_BYTES", "0"},
		{"RACER_GPU_STAGING_BYTES", "1048576"},
		{"RACER_GPU_STAGING_BYTES", "3145728"},
		{"RACER_GPU_STAGING_BYTES", "2MiB"},
		{"RACER_GPU_STAGING_BYTES", "18446744073709551616"},
		{"RACER_GPU_LANDING_BYTES", "1048576"},
		{"RACER_GPU_LANDING_BYTES", "-2097152"},
		{"RACER_GPU_LANDING_BYTES", "+2097152"},
		{"RACER_NVIDIA", "on"},
		{"RACER_ALLOW_SMT", "1"},
		{"RACER_GPU_PCI_P2PDMA", "TRUE"},
		{"RACER_GPU_PCI_P2PDMA", "1"},
		{"RACER_GPU_PCI_P2PDMA", ""},
		{"RACER_REPLICA_SELECTION", "random"},
		{"RACER_ADMISSION_MODE", "on"},
		{"RACER_SHARES", "0"},
		{"RACER_SHARES", "4294967296"},
		{"RACER_SHARES", "18446744073709551615"},
		{"RACER_IO_WORKERS", "0"},
		{"RACER_IO_WORKERS", "65537"},
		{"RACER_CRYPTO_HELPERS", "0"},
		{"RACER_DISK_PAGE_ENTRIES", "1048577"},
		{"RACER_ADMISSION_HISTORY_BYTES", "31"},
		{"RACER_ADMISSION_HISTORY_BYTES", "536870913"},
		{"RACER_ADMISSION_PERIOD_SECS", "0"},
		{"RACER_ADMISSION_PERIOD_SECS", "86401"},
		{"RACER_CHECKPOINT_BYTES", "536870913"},
		{"RACER_DEVICE_BUSY_WAIT_SECS", "3601"},
		{"RACER_RANGE_SPILL_PAGES", "1025"},
		{"RACER_PAGE_HEDGE_DELAY_MS", "0"},
		{"RACER_PAGE_HEDGE_DELAY_MS", "30001"},
		{"RACER_PAGE_HEDGE_SLOTS", "33"},
		{"RACER_PAGE_HEDGE_BYTES", "1073742337"},
		{"RACER_PEER_INFLIGHT_MAX", "0"},
		{"RACER_PEER_INFLIGHT_MAX", "65537"},
		{"RACER_PEER_PER_NEIGHBOR_MAX", "0"},
		{"RACER_PEER_RECEIVE_MAX", "65537"},
		{"RACER_PEER_RECEIVE_QUEUE_MAX", "0"},
		{"RACER_PEER_RECEIVE_QUEUE_MAX", "65537"},
		{"RACER_PEER_RECEIVE_WAIT_MS", "0"},
		{"RACER_PEER_RECEIVE_WAIT_MS", "30001"},
		{"RACER_SLAB_BYTES", "0"},
		{"RACER_SLAB_BYTES", "9223372036854775808"},
		{"RACER_SEGMENT_BYTES", "0"},
		{"RACER_SEGMENT_BYTES", "16797711"},
		{"RACER_FREE_SEGMENT_RESERVE", "0"},
		{"RACER_FREE_SEGMENT_RESERVE", "1048576"},
		{"RACER_PLAINTEXT_BYTES", "68719476737"},
		{"RACER_DIRTY_BYTES", "16777231"},
		{"RACER_REQUEST_CONTEXT_BYTES", "131071"},
		{"RACER_FLIGHTS", "65537"},
		{"RACER_WAITERS_PER_FLIGHT", "4097"},
		{"RACER_QUEUE_ENTRIES", "1"},
		{"RACER_CONNECTIONS_PER_NEIGHBOR", "1025"},
		{"RACER_CLIENT_CONNECTIONS", "65537"},
		{"RACER_PIPES", "65537"},
		{"RACER_RANGE_WINDOW_PAGES", "0"},
		{"RACER_RANGE_WINDOW_PAGES", "65"},
		{"RACER_HEADER_BYTES", "1023"},
		{"RACER_HEADER_BYTES", "32769"},
		{"RACER_PLACEMENT_CACHE_BYTES", "1023"},
		{"RACER_PLACEMENT_CACHE_BYTES", "536870913"},
		{"RACER_CACHED_PATHS", "1048577"},
		{"RACER_PATH_CACHE_BYTES", "536870913"},
		{"RACER_ACTIVE_PATH_SEARCHES", "65"},
		{"RACER_RETAINED_SNAPSHOTS", "1"},
		{"RACER_RETAINED_SNAPSHOTS", "65"},
		{"RACER_METADATA_ENTRIES", "1048577"},
		{"RACER_RELAY_TRANSFERS", "65537"},
		{"RACER_ORIGIN_CONNECTIONS_PER_CACHE", "1025"},
		{"RACER_REQUEST_TIMEOUT_MS", "0"},
		{"RACER_REQUEST_TIMEOUT_MS", "86400001"},
		{"RACER_PEER_ATTEMPT_TIMEOUT_MS", "86400001"},
		{"RACER_READER_STALL_TIMEOUT_MS", "0"},
		{"RACER_SHUTDOWN_TIMEOUT_MS", "3600001"},
		{"RACER_SLAB_BYTES", "1 GiB"},
		{"RACER_WARM_MAX_TTL_SECS", "0"},
		{"RACER_WARM_WINDOW_PAGES", "65"},
		{"RACER_RETAINED_SEGMENT_PERCENT", "101"},
		{"RACER_MAX_THREADS", "2"},
		{"RACER_CONTROL_SEATS", "17"},
		{"RACER_CONTROL_SEATS", "01"},
		{"RACER_SLAB_BYTES", strings.Repeat("1", 4097)},
	} {
		t.Run(tc.key+"="+tc.value, func(t *testing.T) {
			err := validateDataplaneConfig(map[string]string{tc.key: tc.value})
			require.ErrorContains(t, err, tc.key)
			require.ErrorContains(t, err, dataplaneConfigName)
		})
	}
}

func TestValidateDataplaneConfigReportsEveryProblem(t *testing.T) {
	err := validateDataplaneConfig(map[string]string{
		"RACER_ENABLE_GPU": "maybe", "RACER_GPU_STAGING_BYTES": "1", "RACER_IO_WORKERS": "4",
	})
	require.ErrorContains(t, err, "RACER_ENABLE_GPU")
	require.ErrorContains(t, err, "RACER_GPU_STAGING_BYTES")
	require.ErrorContains(t, err, "RACER_CRYPTO_HELPERS must be set together")
}

// An invalid edit fails the plan, so the DaemonSet keeps its last good hash.
func TestInvalidDataplaneConfigKeepsDaemonSet(t *testing.T) {
	env := testEnv(t, cacheObject("cache"))
	initialize(t, env)

	before := liveDataplane(t, env).Spec.Template.Annotations[dataplaneConfigHash]

	config := &corev1.ConfigMap{}
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, dataplaneConfigName), config))
	config.Data["RACER_GPU_STAGING_BYTES"] = "1048576"
	require.NoError(t, env.Client.Update(t.Context(), config))

	_, _, err := (Component{}).Plan(t.Context(), env, nil)
	require.ErrorContains(t, err, "RACER_GPU_STAGING_BYTES")
	require.Equal(t, before, liveDataplane(t, env).Spec.Template.Annotations[dataplaneConfigHash])
}

// One image serves GPU and CPU nodes, so no ConfigMap key selects another
// image. An unknown key such as RACER_CUDA, the renamed RACER_DATAPLANE_CUDA,
// is ignored.
func TestDataplaneImage(t *testing.T) {
	env := testEnv(t, cacheObject("cache"))
	initialize(t, env)

	image := func() string { return liveDataplane(t, env).Spec.Template.Spec.Containers[0].Image }
	require.Equal(t, "mirror.test/unbounded/racer-dataplane:v1", image())

	for _, value := range []string{"true", "false", "yes"} {
		config := &corev1.ConfigMap{}
		require.NoError(t, env.Client.Get(t.Context(), objectKey(env, dataplaneConfigName), config))
		config.Data["RACER_CUDA"] = value
		require.NoError(t, env.Client.Update(t.Context(), config))

		persist(t, env, planPass(t, env))
		require.Equal(t, "mirror.test/unbounded/racer-dataplane:v1", image(), "value %q", value)
	}
}
