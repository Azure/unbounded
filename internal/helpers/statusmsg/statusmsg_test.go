// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package statusmsg

import (
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

func TestTruncate(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		msg      string
		maxBytes int
		want     string
	}{
		{name: "shorter than the limit", msg: "abc", maxBytes: 5, want: "abc"},
		{name: "exactly the limit", msg: "abcde", maxBytes: 5, want: "abcde"},
		{name: "ascii over the limit", msg: "abcdef", maxBytes: 5, want: "abcde"},
		{name: "cut inside a two-byte sequence", msg: "aé", maxBytes: 2, want: "a"},
		{name: "cut after a two-byte sequence", msg: "aéb", maxBytes: 3, want: "aé"},
		{name: "cut inside a four-byte sequence", msg: "a😀", maxBytes: 4, want: "a"},
		{name: "zero limit", msg: "abc", maxBytes: 0, want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := Truncate(tc.msg, tc.maxBytes)
			require.Equal(t, tc.want, got)
			require.LessOrEqual(t, len(got), tc.maxBytes)
			require.True(t, utf8.ValidString(got))
		})
	}
}

func TestTruncateAtMaxBytes(t *testing.T) {
	t.Parallel()

	msg := Truncate(string(make([]byte, MaxBytes+1)), MaxBytes)
	require.Len(t, msg, MaxBytes)
}
