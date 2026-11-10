// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"slices"
	"testing"
)

func TestSnapshotterCommandDispatch(t *testing.T) {
	for _, args := range [][]string{nil, {"--address=/run/test.sock"}, {"-root", "/state"}, {"--help"}} {
		wantErr := errors.New("snapshotter failed")

		var got []string

		cmd := newCmdSnapshotter(func(args []string) error {
			got = args
			return wantErr
		})
		cmd.SetArgs(append([]string{}, args...))

		if err := cmd.Execute(); !errors.Is(err, wantErr) {
			t.Fatalf("error = %v, want %v", err, wantErr)
		}

		if !slices.Equal(got, args) {
			t.Fatalf("arguments = %q, want %q", got, args)
		}
	}
}
