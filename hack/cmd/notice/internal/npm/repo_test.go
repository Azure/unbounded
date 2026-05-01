// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package npm

import (
	"encoding/json"
	"testing"
)

func TestLicenseString(t *testing.T) {
	cases := []struct {
		name string
		json string
		want string
	}{
		{"modern string", `{"license":"MIT"}`, "MIT"},
		{"legacy object", `{"license":{"type":"Apache-2.0","url":"x"}}`, "Apache-2.0"},
		{"plural array", `{"licenses":[{"type":"BSD-3-Clause","url":"x"}]}`, "BSD-3-Clause"},
		{"missing", `{}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var pkg packageJSON
			if err := json.Unmarshal([]byte(tc.json), &pkg); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}

			if got := licenseString(pkg); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRepoBase(t *testing.T) {
	cases := []struct {
		name, json, wantBase string
	}{
		{
			name:     "string url",
			json:     `{"repository":"https://github.com/emotion-js/emotion/tree/main/packages/react"}`,
			wantBase: "https://github.com/emotion-js/emotion",
		},
		{
			name:     "object url with git+ prefix and .git suffix",
			json:     `{"repository":{"type":"git","url":"git+https://github.com/d3/d3-scale.git"}}`,
			wantBase: "https://github.com/d3/d3-scale",
		},
		{
			name:     "ssh url rewritten",
			json:     `{"repository":{"type":"git","url":"ssh://git@github.com/foo/bar.git"}}`,
			wantBase: "https://github.com/foo/bar",
		},
		{
			name:     "missing repository falls back to npm registry",
			json:     `{}`,
			wantBase: "https://www.npmjs.com/package/somepkg",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var pkg packageJSON
			if err := json.Unmarshal([]byte(tc.json), &pkg); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}

			gotBase, _ := repoBase(pkg, "somepkg")
			if gotBase != tc.wantBase {
				t.Errorf("got %q, want %q", gotBase, tc.wantBase)
			}
		})
	}
}
