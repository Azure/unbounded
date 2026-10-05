// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package license contains ecosystem-agnostic helpers for license
// classification, copyright extraction, and license-URL construction.
package license

import (
	"fmt"
	"strings"

	"github.com/google/licensecheck"
)

// Classify recognizes the complete Unicode V3 text or runs google/licensecheck
// against the LICENSE text and returns the
// friendly names of all matched licenses, in source-text order, deduplicated.
// Errors out if no license is recognized.
func Classify(text []byte) ([]string, error) {
	if strings.Join(strings.Fields(string(text)), " ") == strings.Join(strings.Fields(unicodeV3Text), " ") {
		return []string{SPDXFriendly("Unicode-3.0")}, nil
	}

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
