// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDrainFilesGivesConcurrentReadsDistinctFiles(t *testing.T) {
	d, err := newDrainFiles()
	require.NoError(t, err)
	t.Cleanup(d.close)

	first, err := d.get()
	require.NoError(t, err)
	second, err := d.get()
	require.NoError(t, err)
	require.NotSame(t, first, second, "a shared file serializes splice pumps")

	d.put(first)
	d.put(second)

	again, err := d.get()
	require.NoError(t, err)
	require.Same(t, second, again, "idle files are reused")
	d.put(again)
	require.Len(t, d.free, 2)
}

func TestDrainFilesWritesDiscard(t *testing.T) {
	d, err := newDrainFiles()
	require.NoError(t, err)
	t.Cleanup(d.close)

	f, err := d.get()
	require.NoError(t, err)

	n, err := f.Write([]byte("payload"))
	require.NoError(t, err)
	require.Equal(t, 7, n)
	d.put(f)
}

func TestDrainFilesClose(t *testing.T) {
	d, err := newDrainFiles()
	require.NoError(t, err)

	busy, err := d.get()
	require.NoError(t, err)

	idle, err := d.get()
	require.NoError(t, err)
	d.put(idle)

	d.close()

	_, err = idle.Write([]byte("x"))
	require.ErrorIs(t, err, os.ErrClosed, "close closes idle files")

	_, err = busy.Write([]byte("x"))
	require.NoError(t, err, "a file in use stays open until it is returned")

	d.put(busy)

	_, err = busy.Write([]byte("x"))
	require.ErrorIs(t, err, os.ErrClosed, "files returned after close are closed")

	_, err = d.get()
	require.ErrorIs(t, err, os.ErrClosed)

	d.close()
}
