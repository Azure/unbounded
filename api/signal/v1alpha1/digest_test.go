// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"strings"
	"testing"
)

func TestIsSHA256Digest(t *testing.T) {
	t.Parallel()

	valid := "sha256:" + strings.Repeat("3c", 32)
	if !IsSHA256Digest(valid) {
		t.Fatalf("IsSHA256Digest(%q) = false", valid)
	}

	for name, value := range map[string]string{
		"bare hex":         strings.Repeat("3c", 32),
		"uppercase hex":    "sha256:" + strings.Repeat("3C", 32),
		"wrong algorithm":  "sha512:" + strings.Repeat("3c", 32),
		"short":            "sha256:" + strings.Repeat("3c", 31),
		"long":             "sha256:" + strings.Repeat("3c", 33),
		"leading space":    " " + valid,
		"trailing newline": valid + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if IsSHA256Digest(value) {
				t.Fatalf("IsSHA256Digest(%q) = true", value)
			}
		})
	}
}
