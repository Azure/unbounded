// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package mirror

import (
	"errors"
	"fmt"
	"testing"

	"github.com/containerd/errdefs"

	"github.com/Azure/unbounded/internal/gantry/digestpipe"
)

func TestIsDigestMismatchErr(t *testing.T) {
	// Mirrors the production chain: containerdstore wraps "Commit: %w"
	// around containerd's "unexpected commit digest ...: %w" around
	// errdefs.ErrFailedPrecondition. Note the message says "unexpected
	// commit digest", NOT "digest mismatch" - the old substring match
	// would have missed this.
	containerdCommit := fmt.Errorf("containerdstore: Commit: %w",
		fmt.Errorf("unexpected commit digest sha256:aaa, expected sha256:bbb: %w", errdefs.ErrFailedPrecondition))

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "live stream-through digestpipe mismatch",
			err:  fmt.Errorf("peer fetch: %w", digestpipe.ErrDigestMismatch),
			want: true,
		},
		{
			name: "containerd commit failed-precondition",
			err:  containerdCommit,
			want: true,
		},
		{
			name: "bare failed-precondition sentinel",
			err:  errdefs.ErrFailedPrecondition,
			want: true,
		},
		{
			name: "plain digest-mismatch text is no longer matched",
			err:  errors.New("some store: digest mismatch: got x, want y"),
			want: false,
		},
		{
			name: "unrelated transport error",
			err:  errors.New("connection reset by peer"),
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isDigestMismatchErr(tc.err); got != tc.want {
				t.Fatalf("isDigestMismatchErr(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
