// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package unbounded

import "testing"

func TestIsLegacyNamespace(t *testing.T) {
	cases := []struct {
		ns   string
		want bool
	}{
		{LegacyKubeNamespace, true},
		{LegacyNetNamespace, true},
		{"unbounded-kube", true},
		{"unbounded-net", true},
		{"unbounded-system", false},
		{"", false},
		{"default", false},
	}

	for _, tc := range cases {
		if got := IsLegacyNamespace(tc.ns); got != tc.want {
			t.Errorf("IsLegacyNamespace(%q) = %v, want %v", tc.ns, got, tc.want)
		}
	}
}
