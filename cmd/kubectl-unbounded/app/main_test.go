// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"os"
	"testing"

	"github.com/Azure/unbounded/internal/manifesttest"
)

// TestMain refuses to run this package's tests until the manifests they embed
// are rendered. See package manifesttest.
func TestMain(m *testing.M) {
	os.Exit(manifesttest.Main(m, manifesttest.UnboundedOperator))
}
