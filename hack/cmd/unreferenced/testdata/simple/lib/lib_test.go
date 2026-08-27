// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package lib

import "testing"

// HelperInATestFile is declared in a test file. Declarations in test files are
// not candidates, so it must never be reported however dead it is.
func HelperInATestFile() string { return "" }

func TestTestedOnly(t *testing.T) {
	if TestedOnly() == "" {
		t.Fatal("want a value")
	}
}
