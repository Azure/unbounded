// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package license

import "testing"

func TestBuildURL(t *testing.T) {
	cases := []struct {
		base, ref, file, want string
		wantErr               bool
	}{
		{
			"https://github.com/spf13/cobra", "v1.10.2", "LICENSE.txt",
			"https://github.com/spf13/cobra/blob/v1.10.2/LICENSE.txt", false,
		},
		{
			"https://gitlab.com/cznic/sqlite", "v1.49.1", "LICENSE",
			"https://gitlab.com/cznic/sqlite/-/blob/v1.49.1/LICENSE", false,
		},
		{
			"https://cs.opensource.google/go/x/crypto", "v0.50.0", "LICENSE",
			"https://cs.opensource.google/go/x/crypto/+/refs/tags/v0.50.0:LICENSE", false,
		},
		{
			"https://bitbucket.org/foo/bar", "main", "LICENSE",
			"https://bitbucket.org/foo/bar/src/main/LICENSE", false,
		},
		{
			"https://example.com/unknown/forge", "v1", "LICENSE",
			"https://example.com/unknown/forge", false,
		},
		{"", "v1", "LICENSE", "", true},
	}
	for _, tc := range cases {
		got, err := BuildURL(tc.base, tc.ref, tc.file)
		if tc.wantErr {
			if err == nil {
				t.Errorf("BuildURL(%q, %q, %q) want error", tc.base, tc.ref, tc.file)
			}

			continue
		}

		if err != nil {
			t.Errorf("BuildURL(%q, %q, %q) error: %v", tc.base, tc.ref, tc.file, err)
			continue
		}

		if got != tc.want {
			t.Errorf("BuildURL(%q, %q, %q) = %q, want %q", tc.base, tc.ref, tc.file, got, tc.want)
		}
	}
}
