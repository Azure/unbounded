// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

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
