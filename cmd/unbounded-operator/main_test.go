// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import "testing"

func TestEnvBoolDefault(t *testing.T) {
	cases := []struct {
		name     string
		set      bool
		value    string
		fallback bool
		want     bool
	}{
		{name: "unset uses fallback true", set: false, fallback: true, want: true},
		{name: "unset uses fallback false", set: false, fallback: false, want: false},
		{name: "empty uses fallback", set: true, value: "", fallback: true, want: true},
		{name: "explicit false overrides", set: true, value: "false", fallback: true, want: false},
		{name: "explicit true overrides", set: true, value: "true", fallback: false, want: true},
		{name: "unparseable uses fallback", set: true, value: "notabool", fallback: true, want: true},
	}

	const key = "UNBOUNDED_TEST_BOOL"

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set {
				t.Setenv(key, tc.value)
			}

			if got := envBoolDefault(key, tc.fallback); got != tc.want {
				t.Fatalf("envBoolDefault = %v, want %v", got, tc.want)
			}
		})
	}
}
