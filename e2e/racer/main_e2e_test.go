//go:build e2e

// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"os"
	"testing"
)

// TestMain deletes the shared cluster after every test has run. It must live
// in a _test.go file, or go test does not call it.
func TestMain(m *testing.M) {
	code := m.Run()

	teardownShared()

	os.Exit(code)
}
