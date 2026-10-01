// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"testing"
)

func TestRejectInitializeCLI(t *testing.T) {
	before := os.Args

	t.Cleanup(func() { os.Args = before })

	for _, args := range [][]string{{"racer-controller", "initialize"}, {"racer-controller", "other"}, {"racer-controller", "initialize", "extra"}} {
		os.Args = args
		if err := run(); err == nil || err.Error() != "usage: racer-controller" {
			t.Fatalf("args %v: %v", args, err)
		}
	}
}
