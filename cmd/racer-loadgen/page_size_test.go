// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"io"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPageSizeValidation(t *testing.T) {
	for _, size := range []int{0, 4 << 10, 8 << 20, 16 << 20} {
		require.NoError(t, validatePageSize(size), "size %d", size)
	}

	for _, size := range []int{-1, 2048, 3072, 16<<20 + 1, 32 << 20} {
		require.Error(t, validatePageSize(size), "size %d", size)
	}
}

func TestPageSizeOptionAndSDKConfigs(t *testing.T) {
	opts, err := parseOptions([]string{"--backend=uds", "--cache=test", "--page-size=4096"}, io.Discard)
	require.NoError(t, err)
	require.Equal(t, 4<<10, opts.pull.PageSize)

	client := sdkClientConfig(opts.pull, 4)
	require.Equal(t, 4<<10, client.PageSize)

	origin := sdkOriginConfig(opts.pull, 4)
	require.Equal(t, 4<<10, origin.PageSize)
	require.Equal(t, "test", origin.Cache)
	require.True(t, origin.RecoverStaleSocket)
}

func TestPageSizeInvalidFlagAndPuller(t *testing.T) {
	for _, args := range [][]string{
		{"--page-size=2048"},
		{"--page-size=3072"},
		{"--page-size=33554432"},
	} {
		_, err := parseOptions(args, io.Discard)
		require.ErrorContains(t, err, "page-size must be a power of two")
	}

	opts := pullTestOptions("http://localhost")
	opts.PageSize = 3072

	_, err := newPuller(&syntheticImage{}, opts, pullTestMetrics())
	require.ErrorContains(t, err, "page-size must be a power of two")
}

func TestPageSizeDiagnosticDefault(t *testing.T) {
	opts := pullTestOptions("http://localhost")
	require.Equal(t, maxPageSize, effectivePageSize(opts.PageSize))

	opts.PageSize = 4 << 10
	require.Equal(t, 4<<10, effectivePageSize(opts.PageSize))
}
