// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package reset

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCleanupRoutesPropagatesFailures(t *testing.T) {
	t.Parallel()

	for _, failure := range []string{"-4 -N -j rule show", "-4 rule del table 51820", "-4 -N -j route show table all", "-4 route flush table 51820", "-6 -N -j rule show", "-6 rule del table 51820", "-6 -N -j route show table all", "-6 route flush table 51820"} {
		t.Run(failure, func(t *testing.T) {
			t.Parallel()

			injected := errors.New("injected network failure")
			task := &cleanupRoutes{log: slog.New(slog.DiscardHandler), output: func(_ context.Context, args ...string) (string, error) {
				if strings.Join(args, " ") == failure {
					return "", injected
				}

				return `[{"table":51820}]`, nil
			}}
			require.ErrorIs(t, task.Do(t.Context()), injected)
		})
	}
}

func TestCleanupRoutesOnlyRemovesEnumeratedOwnedState(t *testing.T) {
	t.Parallel()

	var mutations []string

	task := &cleanupRoutes{log: slog.New(slog.DiscardHandler), output: func(_ context.Context, args ...string) (string, error) {
		if args[1] == "-N" {
			return `[{"table":"main"},{"table":51819},{"table":51820},{"table":"51820"},{"table":51899},{"table":51900}]`, nil
		}

		mutations = append(mutations, strings.Join(args, " "))

		return "", nil
	}}
	require.NoError(t, task.Do(t.Context()))
	require.Equal(t, []string{
		"-4 rule del table 51820", "-4 rule del table 51820", "-4 rule del table 51899",
		"-4 route flush table 51820", "-4 route flush table 51899",
		"-6 rule del table 51820", "-6 rule del table 51820", "-6 rule del table 51899",
		"-6 route flush table 51820", "-6 route flush table 51899",
	}, mutations)
}

func TestCleanupRoutesAbsenceAndInvalidInspection(t *testing.T) {
	t.Parallel()

	for _, output := range []string{"[]", "invalid", `[{"table":"unexpected-name"}]`, `[{"table":{}}]`} {
		t.Run(output, func(t *testing.T) {
			t.Parallel()

			task := &cleanupRoutes{log: slog.New(slog.DiscardHandler), output: func(_ context.Context, args ...string) (string, error) {
				require.Contains(t, args, "show", "absence or invalid inspection must never cause mutation")
				return output, nil
			}}

			err := task.Do(t.Context())
			if output == "[]" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}
