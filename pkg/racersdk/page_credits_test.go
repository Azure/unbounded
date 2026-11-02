// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import "testing"

func TestClientPageCredits(t *testing.T) {
	for name, tc := range map[string]struct {
		in   int
		want int
		ok   bool
	}{
		"default": {0, 2, true},
		"one":     {1, 1, true},
		"max":     {64, 64, true},
		"over":    {65, 0, false},
		"neg":     {-1, 0, false},
	} {
		t.Run(name, func(t *testing.T) {
			c, err := newClient(ClientConfig{Cache: "blobs", PageCredits: tc.in}, "/nonexistent/socket")
			if !tc.ok {
				if err == nil {
					t.Fatal("expected an error")
				}

				return
			}

			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()

			if c.credits != tc.want {
				t.Fatalf("credits = %d, want %d", c.credits, tc.want)
			}
		})
	}
}
