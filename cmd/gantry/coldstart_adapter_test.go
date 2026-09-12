// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/gantry/coldstart"
	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/mirror"
)

type blockingColdStartEngine struct{}

func (blockingColdStartEngine) Resolve(ctx context.Context, _ digest.Digest, _ ifaces.OriginRefKind, _, _ string, _ int64) (*coldstart.Resolution, error) {
	<-ctx.Done()

	return nil, ctx.Err()
}

func TestColdStartAdapterDeadlineBecomesExhausted(t *testing.T) {
	adapter := coldStartAdapter{r: blockingColdStartEngine{}, timeout: time.Millisecond}
	d := digest.MustParse("sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")

	_, err := adapter.Resolve(context.Background(), d, ifaces.KindBlob, "registry.example.com", "repo/image", 0)
	if !errors.Is(err, mirror.ErrColdStartDeadlineExceeded) {
		t.Fatalf("Resolve error = %v, want ErrColdStartDeadlineExceeded", err)
	}
}

func TestColdStartAdapterPreservesCallerCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	adapter := coldStartAdapter{r: blockingColdStartEngine{}, timeout: time.Minute}
	d := digest.MustParse("sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")

	_, err := adapter.Resolve(ctx, d, ifaces.KindBlob, "registry.example.com", "repo/image", 0)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Resolve error = %v, want context.Canceled", err)
	}
}
