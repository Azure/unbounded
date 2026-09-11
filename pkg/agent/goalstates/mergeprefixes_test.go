// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package goalstates

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestMergeHostPrefixes covers teardown's prefix discovery, which has to work
// from whichever sources happen to exist: the installation record is present
// from before the first mutation, the applied config only once the node ran,
// and a host provisioned by an older agent has neither.
func TestMergeHostPrefixes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		candidates []string
		want       []string
	}{
		{
			name:       "no candidates still sweeps the default",
			candidates: nil,
			want:       []string{DefaultHostPrefix},
		},
		{
			name:       "empty candidates are ignored",
			candidates: []string{"", "   "},
			want:       []string{DefaultHostPrefix},
		},
		{
			name:       "custom prefix is swept alongside the default",
			candidates: []string{"/opt/unbounded"},
			want:       []string{"/opt/unbounded", DefaultHostPrefix},
		},
		{
			name:       "duplicate sources collapse",
			candidates: []string{"/opt/unbounded", "/opt/unbounded"},
			want:       []string{"/opt/unbounded", DefaultHostPrefix},
		},
		{
			// A host whose prefix changed between installs carries files under
			// both, so both have to be swept.
			name:       "distinct prefixes are all swept",
			candidates: []string{"/opt/a", "/opt/b"},
			want:       []string{"/opt/a", DefaultHostPrefix, "/opt/b"},
		},
		{
			name:       "the default as a candidate does not duplicate",
			candidates: []string{DefaultHostPrefix},
			want:       []string{DefaultHostPrefix},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.want, MergeHostPrefixes(tc.candidates...))
		})
	}
}
