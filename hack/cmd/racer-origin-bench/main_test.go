// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReadCPU(t *testing.T) {
	c := readCPU(os.Getpid())
	require.Positive(t, c.Threads)
	require.InDelta(t, c.UserSeconds+c.SystemSeconds, c.Seconds, 1e-9)

	require.Equal(t, CPU{}, readCPU(-1))
}

func TestSummarize(t *testing.T) {
	dir := t.TempDir()

	for i, r := range []Result{
		{Variant: "before", Mode: "cached", ObjectSize: 64 << 20, Concurrency: 8, CPUPerGiB: map[string]float64{"racer+origin": 2}},
		{Variant: "after", Mode: "cached", ObjectSize: 64 << 20, Concurrency: 8, CPUPerGiB: map[string]float64{"racer+origin": 1}},
	} {
		data, err := json.Marshal(r)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(dir, string(rune('a'+i))+".json"), data, 0o600))
	}

	require.NoError(t, summarizeCommand([]string{"-dir", dir}))

	require.NoError(t, os.WriteFile(filepath.Join(dir, "bad.json"), []byte("{"), 0o600))
	require.Error(t, summarizeCommand([]string{"-dir", dir}))
}

func TestRunRejectsInvalidFlags(t *testing.T) {
	require.Error(t, runCommand([]string{"-variant", "before"}))
	require.Error(t, runCommand([]string{"-variant", "sideways", "-racer", "r", "-control", "c", "-fake", "f", "-work", "w", "-out", "o"}))
	require.Error(t, runCommand([]string{"-variant", "after", "-racer", "r", "-control", "c", "-fake", "f", "-work", "w", "-out", "o", "-total-bytes", "1"}))
}
