// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package statusmsg

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	metav1validation "k8s.io/apimachinery/pkg/apis/meta/v1/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
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
		{name: "empty message", msg: "", maxBytes: 0, want: ""},
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

func TestTruncatePanicsOnNegativeMaxBytes(t *testing.T) {
	t.Parallel()

	require.Panics(t, func() { Truncate("abc", -1) })
	require.Panics(t, func() { Truncate("", -1) })
}

// The condition limit is a copy of an unexported Kubernetes constant, so these
// cases check it against Kubernetes' own validation.
func TestTruncateCondition(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		msg   string
		valid bool
	}{
		{name: "at the limit", msg: strings.Repeat("a", conditionMaxBytes), valid: true},
		{name: "one byte over the limit", msg: strings.Repeat("a", conditionMaxBytes+1), valid: false},
		{name: "truncated ascii", msg: TruncateCondition(strings.Repeat("a", conditionMaxBytes+1)), valid: true},
		{name: "truncated multi-byte", msg: TruncateCondition(strings.Repeat("é", conditionMaxBytes)), valid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			errs := metav1validation.ValidateCondition(metav1.Condition{
				Type:               "Ready",
				Status:             metav1.ConditionFalse,
				Reason:             "Failed",
				Message:            tc.msg,
				LastTransitionTime: metav1.Now(),
			}, field.NewPath("status", "conditions"))
			require.Equal(t, tc.valid, len(errs) == 0, "validation errors: %v", errs)
			require.True(t, utf8.ValidString(tc.msg))
		})
	}
}
