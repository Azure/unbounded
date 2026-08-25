// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cdsub

import (
	"context"
)

// NoOpSource is an ImageSource that produces no events. Useful when
// containerd is unavailable (CI, darwin development) or when the agent
// is configured to skip image-event subscription. The Subscriber's
// reconnect loop will quietly call List -> Subscribe -> wait-on-ctx -> exit,
// keeping the announce machinery exercised at zero cost.
type NoOpSource struct{}

// List always returns an empty event slice.
func (NoOpSource) List(_ context.Context) ([]ImageEvent, error) { return nil, nil }

// Subscribe returns a channel that is closed only when ctx is canceled.
func (NoOpSource) Subscribe(ctx context.Context) (<-chan ImageEvent, error) {
	ch := make(chan ImageEvent)

	go func() {
		<-ctx.Done()
		close(ch)
	}()

	return ch, nil
}
