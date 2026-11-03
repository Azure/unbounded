// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package isolation

import (
	"context"
	"io"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestJournalTailArgs(t *testing.T) {
	t.Parallel()

	require.Empty(t, JournalTailArgs(LogOptions{}))
	require.Equal(t, []string{"--follow"}, JournalTailArgs(LogOptions{Follow: true}))
	require.Equal(t, []string{"--lines=20"}, JournalTailArgs(LogOptions{Lines: 20}))
	require.Equal(t, []string{"--follow", "--lines=5"}, JournalTailArgs(LogOptions{Follow: true, Lines: 5}))
	require.Empty(t, JournalTailArgs(LogOptions{Lines: -1}), "a non-positive count means no limit")
}

func TestStartLogStreamReadsOutputAndReapsOnClose(t *testing.T) {
	t.Parallel()

	r, err := StartLogStream(t.Context(), "sh", "-c", "echo line")
	require.NoError(t, err)

	data, err := io.ReadAll(r)
	require.NoError(t, err)
	require.Equal(t, "line\n", string(data))
	require.NoError(t, r.Close())
}

// journalctl against a machine that does not exist prints a reason on stderr and
// exits non-zero. A stream that ended that way must say so, or `logs` prints
// nothing and succeeds.
func TestStartLogStreamReportsACommandThatFailed(t *testing.T) {
	t.Parallel()

	r, err := StartLogStream(t.Context(), "sh", "-c", "echo 'No journal files were found.' >&2; exit 3")
	require.NoError(t, err)

	data, err := io.ReadAll(r)
	require.NoError(t, err)
	require.Empty(t, data)

	err = r.Close()
	require.Error(t, err)
	require.ErrorContains(t, err, "No journal files were found.")
	require.ErrorContains(t, err, "exit status 3")
}

func TestStartLogStreamKeepsTheTailOfALongStderr(t *testing.T) {
	t.Parallel()

	r, err := StartLogStream(t.Context(), "sh", "-c", "i=0; while [ $i -lt 5000 ]; do echo noise-$i >&2; i=$((i+1)); done; echo the-reason >&2; exit 1")
	require.NoError(t, err)

	_, err = io.ReadAll(r)
	require.NoError(t, err)

	err = r.Close()
	require.Error(t, err)
	require.ErrorContains(t, err, "the-reason")
	require.Less(t, len(err.Error()), 8192, "the report is bounded")
}

// Stopping a stream that is still running, by canceling it or closing it
// early, is not a failure.
func TestStartLogStreamStoppedByTheCallerIsNotAnError(t *testing.T) {
	t.Parallel()

	t.Run("context canceled", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithCancel(t.Context())

		r, err := StartLogStream(ctx, "sh", "-c", "echo up; exec sleep 30")
		require.NoError(t, err)

		buf := make([]byte, 3)
		_, err = io.ReadFull(r, buf)
		require.NoError(t, err)

		cancel()

		_, _ = io.Copy(io.Discard, r) //nolint:errcheck // draining a killed stream
		require.NoError(t, r.Close())
	})

	t.Run("closed before the end", func(t *testing.T) {
		t.Parallel()

		r, err := StartLogStream(t.Context(), "sh", "-c", "exec yes up")
		require.NoError(t, err)

		buf := make([]byte, 3)
		_, err = io.ReadFull(r, buf)
		require.NoError(t, err)
		require.NoError(t, r.Close())
	})
}

func TestStartLogStreamReportsAMissingCommand(t *testing.T) {
	t.Parallel()

	_, err := StartLogStream(t.Context(), "/nonexistent/unbounded-test-command")
	require.Error(t, err)
}
