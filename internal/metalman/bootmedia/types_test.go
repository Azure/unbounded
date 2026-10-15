// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package bootmedia

import "testing"

func TestPrimerName(t *testing.T) {
	for name, want := range map[string]string{
		"ub-0123abcd.iso": "up-0123abcd.iso",
		"ub-.iso":         "up-.iso",
		"up-0123abcd.iso": "",
		"other.iso":       "",
		"":                "",
	} {
		if got := PrimerName(name); got != want {
			t.Errorf("PrimerName(%q) = %q, want %q", name, got, want)
		}
	}
}
