// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package license contains ecosystem-agnostic helpers for license
// classification, copyright extraction, and license-URL construction.
package license

import (
	"fmt"

	"github.com/google/licensecheck"
)

// Classify runs google/licensecheck against the LICENSE text and returns the
// friendly names of all matched licenses, in source-text order, deduplicated.
// Errors out if no license is recognized.
func Classify(text []byte) ([]string, error) {
	cov := licensecheck.Scan(text)
	if len(cov.Match) == 0 {
		return nil, fmt.Errorf("license not recognized by licensecheck")
	}

	seen := map[string]bool{}

	var out []string

	for _, m := range cov.Match {
		friendly := SPDXFriendly(m.ID)
		if seen[friendly] {
			continue
		}

		seen[friendly] = true

		out = append(out, friendly)
	}

	return out, nil
}
