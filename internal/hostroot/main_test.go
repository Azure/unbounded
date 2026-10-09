// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package hostroot

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	// The tests lay hosts out under their temporary directories, which the
	// user running them owns. TestCheckPath passes owners explicitly, so it
	// still covers a directory root does not own.
	trustedOwners = append(trustedOwners, uint32(os.Getuid())) //nolint:gosec // A uid fits in uint32.

	os.Exit(m.Run())
}
