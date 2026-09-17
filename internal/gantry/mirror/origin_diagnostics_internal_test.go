// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package mirror

import (
	"context"
	"errors"
	"testing"
)

type originTimeoutError struct{}

func (originTimeoutError) Error() string   { return "timed out" }
func (originTimeoutError) Timeout() bool   { return true }
func (originTimeoutError) Temporary() bool { return true }

func TestOriginDeadlineOwner(t *testing.T) {
	callerCtx, callerCancel := context.WithCancel(context.Background())
	callerCancel()

	tests := []struct {
		name string
		ctx  context.Context
		err  error
		want string
	}{
		{name: "caller", ctx: callerCtx, err: context.Canceled, want: "caller"},
		{name: "transport", ctx: context.Background(), err: originTimeoutError{}, want: "transport"},
		{name: "none", ctx: context.Background(), err: errors.New("failed"), want: "none"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := originDeadlineOwner(test.ctx, test.err); got != test.want {
				t.Fatalf("owner = %q; want %q", got, test.want)
			}
		})
	}
}
