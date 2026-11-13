// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"

	"github.com/Azure/unbounded/internal/racer/workload"
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
			"RACER_MAX_THREADS": "0004", "RACER_PREFETCH_MAX_TICKETS": "65536", "RACER_RETAINED_SEGMENT_PERCENT": "1",
			"RACER_RANGE_SPILL_PAGES": "0", "RACER_SHARES": "4294967295",
		},
		"range minimums": {
			"RACER_IO_WORKERS": "1", "RACER_CRYPTO_HELPERS": "1", "RACER_SHARES": "1",
			"RACER_ADMISSION_HISTORY_BYTES": "32", "RACER_DEVICE_BUSY_WAIT_SECS": "0",
			"RACER_PAGE_HEDGE_SLOTS": "0", "RACER_PEER_RECEIVE_MAX": "0",
			"RACER_SLAB_BYTES_PER_WORKER": "1", "RACER_SEGMENT_BYTES": "16797712", "RACER_FREE_SEGMENT_RESERVE_PER_WORKER": "1",
			"RACER_DIRTY_BYTES": "16777232", "RACER_REQUEST_CONTEXT_BYTES": "131072",
			"RACER_QUEUE_ENTRIES": "2", "RACER_HEADER_BYTES": "1024", "RACER_PLACEMENT_CACHE_BYTES": "1024",
			"RACER_RETAINED_SNAPSHOTS": "2", "RACER_RANGE_WINDOW_PAGES": "1", "RACER_REQUEST_TIMEOUT_MS": "1",
			"RACER_PREFETCH_TICKET_RETENTION_SECS": "0", "RACER_PREFETCH_ORIGIN_BYTES_PER_SEC": "0",
			"RACER_PREFETCH_DEFAULT_PENDING_BYTES": "16777216",
		},
		"range maximums": {
			"RACER_IO_WORKERS": "65536", "RACER_CRYPTO_HELPERS": "65536", "RACER_DISK_PAGE_ENTRIES": "1048576",
			"RACER_ADMISSION_HISTORY_BYTES": "536870912", "RACER_ADMISSION_PERIOD_SECS": "86400",
			"RACER_CHECKPOINT_BYTES": "536870912", "RACER_DEVICE_BUSY_WAIT_SECS": "3600",
			"RACER_RANGE_SPILL_PAGES": "1024", "RACER_PAGE_HEDGE_DELAY_MS": "30000", "RACER_PAGE_HEDGE_SLOTS": "32",
			"RACER_PAGE_HEDGE_BYTES": "1073742336", "RACER_PEER_INFLIGHT_MAX": "65536",
			"RACER_PEER_RECEIVE_MAX": "65536", "RACER_PEER_RECEIVE_QUEUE_MAX": "65536",
			"RACER_PEER_RECEIVE_WAIT_MS": "30000", "RACER_SLAB_BYTES_PER_WORKER": "9223372036854775807",
			"RACER_FREE_SEGMENT_RESERVE_PER_WORKER": "1048575", "RACER_PLAINTEXT_BYTES": "68719476736",
			"RACER_FLIGHTS": "65536", "RACER_WAITERS_PER_FLIGHT": "4096", "RACER_QUEUE_ENTRIES": "65536",
			"RACER_NEIGHBOR_CONNECTIONS_PER_WORKER": "1024", "RACER_PIPES": "65536", "RACER_RANGE_WINDOW_PAGES": "64",
			"RACER_HEADER_BYTES": "32768", "RACER_PLACEMENT_CACHE_BYTES": "536870912",
			"RACER_CACHED_PATHS": "1048576", "RACER_ACTIVE_PATH_SEARCHES_PER_WORKER": "64", "RACER_RETAINED_SNAPSHOTS": "64",
			"RACER_CACHE_ORIGIN_CONNECTIONS_PER_WORKER": "1024", "RACER_REQUEST_TIMEOUT_MS": "86400000",
			"RACER_SHUTDOWN_TIMEOUT_MS":     "3600000",
			"RACER_PREFETCH_MAX_BODY_BYTES": "67108864", "RACER_PREFETCH_MAX_REFS": "1048576",
			"RACER_PREFETCH_NODE_PENDING_BYTES": "4503599627370496", "RACER_PREFETCH_CONCURRENT_FILLS": "1024",
		},
		"thread split":               {"RACER_IO_WORKERS": "2", "RACER_CRYPTO_HELPERS": "1", "RACER_CONTROL_SEATS": "auto"},
		"keys of others are ignored": {"OTHER_SETTING": "x", "DST_SEEDS": "1"},
		"per-worker budgets":         {"RACER_FLIGHTS_PER_WORKER": "64", "RACER_PLAINTEXT_BYTES": "268435456"},
		"empty means unset":          {"RACER_OBJECT_IMAGE": "", "RACER_AZURE_FEDERATED_TOKEN_FILE": ""},
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
		{"RACER_SLAB_BYTES_PER_WORKER", "0"},
		{"RACER_SLAB_BYTES_PER_WORKER", "9223372036854775808"},
		{"RACER_SEGMENT_BYTES", "0"},
		{"RACER_SEGMENT_BYTES", "16797711"},
		{"RACER_FREE_SEGMENT_RESERVE_PER_WORKER", "0"},
		{"RACER_FREE_SEGMENT_RESERVE_PER_WORKER", "1048576"},
		{"RACER_PLAINTEXT_BYTES", "68719476737"},
		{"RACER_DIRTY_BYTES", "16777231"},
		{"RACER_REQUEST_CONTEXT_BYTES", "131071"},
		{"RACER_FLIGHTS", "65537"},
		{"RACER_WAITERS_PER_FLIGHT", "4097"},
		{"RACER_QUEUE_ENTRIES", "1"},
		{"RACER_NEIGHBOR_CONNECTIONS_PER_WORKER", "1025"},
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
		{"RACER_ACTIVE_PATH_SEARCHES_PER_WORKER", "65"},
		{"RACER_RETAINED_SNAPSHOTS", "1"},
		{"RACER_RETAINED_SNAPSHOTS", "65"},
		{"RACER_METADATA_ENTRIES", "1048577"},
		{"RACER_RELAY_TRANSFERS", "65537"},
		{"RACER_CACHE_ORIGIN_CONNECTIONS_PER_WORKER", "1025"},
		{"RACER_REQUEST_TIMEOUT_MS", "0"},
		{"RACER_REQUEST_TIMEOUT_MS", "86400001"},
		{"RACER_PEER_ATTEMPT_TIMEOUT_MS", "86400001"},
		{"RACER_READER_STALL_TIMEOUT_MS", "0"},
		{"RACER_SHUTDOWN_TIMEOUT_MS", "3600001"},
		{"RACER_SLAB_BYTES_PER_WORKER", "1 GiB"},
		{"RACER_PREFETCH_MAX_BODY_BYTES", "4095"},
		{"RACER_PREFETCH_MAX_BODY_BYTES", "67108865"},
		{"RACER_PREFETCH_MAX_WAIT_SECS", "0"},
		{"RACER_PREFETCH_MAX_WAIT_SECS", "601"},
		{"RACER_PREFETCH_MAX_PENDING_BYTES", "16777215"},
		{"RACER_PREFETCH_NODE_PENDING_BYTES", "4503599627370497"},
		{"RACER_PREFETCH_ORIGIN_BYTES_PER_SEC", "1099511627777"},
		{"RACER_PREFETCH_MAX_ATTEMPTS", "33"},
		{"RACER_RETAINED_SEGMENT_PERCENT", "101"},
		{"RACER_MAX_THREADS", "2"},
		{"RACER_CONTROL_SEATS", "17"},
		{"RACER_CONTROL_SEATS", "01"},
		{"RACER_SOMETHING_ELSE", "x"},
		{"RACER_WARM_MAX_TTL_SECS", "0"},
		{"RACER_SLAB_BYTES", "1073741824"},
		{"RACER_SERVICE_ACCOUNT_TOKEN", "/var/run/racer-token/token"},
		{"RACER_SLAB_DIRECTORY", ""},
		{"RACER_SLAB_DIRECTORY", "/a\nb"},
		{"RACER_SLAB_BYTES_PER_WORKER", strings.Repeat("1", 4097)},
	} {
		t.Run(tc.key+"="+tc.value, func(t *testing.T) {
			err := validateDataplaneConfig(map[string]string{tc.key: tc.value})
			require.ErrorContains(t, err, tc.key)
			require.ErrorContains(t, err, dataplaneConfigName)
		})
	}
}

func TestValidateDataplaneMaxThreadsRDMAOwner(t *testing.T) {
	for _, tc := range []struct {
		rdma, threads string
		ok            bool
	}{
		{"", "3", false},
		{"auto", "3", false},
		{"true", "3", false},
		{"false", "3", true},
		{"false", "2", false},
		{"", "4", true},
		{"auto", "4", true},
		{"true", "4", true},
	} {
		t.Run(tc.rdma+"/"+tc.threads, func(t *testing.T) {
			data := map[string]string{"RACER_MAX_THREADS": tc.threads}
			if tc.rdma != "" {
				data["RACER_ENABLE_RDMA"] = tc.rdma
			}

			err := validateDataplaneConfig(data)
			if tc.ok {
				require.NoError(t, err)

				return
			}

			require.ErrorContains(t, err, "RACER_MAX_THREADS")

			if tc.rdma != "false" {
				require.ErrorContains(t, err, "RDMA owner")
			}
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
// fails the plan, since the dataplane would refuse it, and the image stays.
func TestDataplaneImage(t *testing.T) {
	env := testEnv(t, cacheObject("cache"))
	initialize(t, env)

	image := func() string { return liveDataplane(t, env).Spec.Template.Spec.Containers[0].Image }
	require.Equal(t, "mirror.test/unbounded/racer:v1", image())

	for _, value := range []string{"true", "false", "yes"} {
		config := &corev1.ConfigMap{}
		require.NoError(t, env.Client.Get(t.Context(), objectKey(env, dataplaneConfigName), config))
		config.Data["RACER_CUDA"] = value
		require.NoError(t, env.Client.Update(t.Context(), config))

		_, _, err := (Component{}).Plan(t.Context(), env, nil)
		require.ErrorContains(t, err, "RACER_CUDA is not a recognized setting")
		require.Equal(t, "mirror.test/unbounded/racer:v1", image(), "value %q", value)
	}
}

func TestDataplaneSettingsTableIsEmbedded(t *testing.T) {
	require.Contains(t, dataplaneSettings, workload.NVIDIAKey)
	require.Contains(t, dataplaneSettings, workload.ControlSeatsKey)
	require.Equal(t, "operator", dataplaneSettings[workload.NVIDIAKey].Owner)
	require.Equal(t, "per-worker", dataplaneSettings["RACER_FLIGHTS_PER_WORKER"].Scope)
	require.Equal(t, "node", dataplaneSettings["RACER_FLIGHTS"].Scope)
}

// Every default the dataplane would use passes the operator's checks.
func TestDataplaneSettingsDefaultsAreAccepted(t *testing.T) {
	for name, setting := range dataplaneSettings {
		if setting.Default == nil {
			continue
		}

		require.Empty(t, checkDataplaneValue(setting, *setting.Default), name)
	}
}

func TestDataplaneSettingsBoundsAndSteps(t *testing.T) {
	staging := dataplaneSettings["RACER_GPU_STAGING_BYTES"]
	require.Empty(t, checkDataplaneValue(staging, "4194304"))
	require.Contains(t, checkDataplaneValue(staging, "3145728"), "multiple of 2097152")
	require.Contains(t, checkDataplaneValue(staging, "0"), "from 2097152")

	for name, setting := range dataplaneSettings {
		if setting.Kind != "unsigned" || setting.Max == "18446744073709551615" {
			continue
		}

		maximum, err := strconv.ParseUint(setting.Max, 10, 64)
		require.NoError(t, err)
		require.NotEmpty(t, checkDataplaneValue(setting, strconv.FormatUint(maximum+1, 10)), name)
	}
}

func TestValidateDataplaneConfigRejectsBothBudgetForms(t *testing.T) {
	err := validateDataplaneConfig(map[string]string{
		"RACER_FLIGHTS": "128", "RACER_FLIGHTS_PER_WORKER": "64",
	})
	require.ErrorContains(t, err, "RACER_FLIGHTS and RACER_FLIGHTS_PER_WORKER must not both be set")
}

func TestCheckDataplaneValueUnsignedBounds(t *testing.T) {
	unbounded := dataplaneSetting{Name: "RACER_TEST", Kind: "unsigned"}
	require.Empty(t, checkDataplaneValue(unbounded, "0"))
	require.Empty(t, checkDataplaneValue(unbounded, "18446744073709551615"))
	require.NotEmpty(t, checkDataplaneValue(unbounded, "x"))

	for field, setting := range map[string]dataplaneSetting{
		"min":  {Name: "RACER_TEST", Kind: "unsigned", Min: "a", Max: "10", Step: "1"},
		"max":  {Name: "RACER_TEST", Kind: "unsigned", Min: "0", Max: "-1", Step: "1"},
		"step": {Name: "RACER_TEST", Kind: "unsigned", Min: "0", Max: "10", Step: "1.5"},
	} {
		t.Run(field, func(t *testing.T) {
			rule := checkDataplaneValue(setting, "5")
			require.Contains(t, rule, "RACER_TEST")
			require.Contains(t, rule, "invalid "+field+" bound")
		})
	}
}
