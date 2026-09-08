// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package utilio

import (
	"math"
	"strings"
	"testing"
)

func TestInstallFileWithLimitedSizeRejectsOverflowingLimit(t *testing.T) {
	t.Parallel()

	path := t.TempDir() + "/installed"
	if err := InstallFileWithLimitedSize(path, strings.NewReader("content"), 0o600, math.MaxInt64); err == nil {
		t.Fatal("InstallFileWithLimitedSize error = nil")
	}
}
