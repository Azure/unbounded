// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package license

import (
	"slices"
	"strings"
	"testing"
)

func TestClassifyUnicodeV3(t *testing.T) {
	for _, tc := range []struct {
		name      string
		text      string
		wantError bool
	}{
		{name: "complete", text: unicodeV3Text},
		{name: "rewrapped", text: strings.Join(strings.Fields(unicodeV3Text), "\r\n\t")},
		{name: "title only", text: "UNICODE LICENSE V3", wantError: true},
		{name: "modified terms", text: strings.Replace(unicodeV3Text, "free of charge", "for a fee", 1), wantError: true},
		{name: "additional terms", text: unicodeV3Text + "Additional restrictions apply.", wantError: true},
		{name: "empty", wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Classify([]byte(tc.text))
			if tc.wantError {
				if err == nil {
					t.Fatalf("Classify() = %v, expected rejection", got)
				}

				return
			}

			if err != nil {
				t.Fatal(err)
			}

			if !slices.Equal(got, []string{"Unicode License v3"}) {
				t.Fatalf("Classify() = %v, want Unicode License v3", got)
			}
		})
	}
}
