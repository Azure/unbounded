// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package license

import "testing"

func TestSPDXFriendly(t *testing.T) {
	cases := []struct{ in, want string }{
		{"MIT", "MIT License"},
		{"Apache-2.0", "Apache License, Version 2.0"},
		{"BSD-3-Clause", "BSD 3-Clause License"},
		{"ISC", "ISC License"},
		{"MIT OR Apache-2.0", "MIT License"},
		{"NotARealLicense", "NotARealLicense"},
	}
	for _, tc := range cases {
		if got := SPDXFriendly(tc.in); got != tc.want {
			t.Errorf("SPDXFriendly(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
