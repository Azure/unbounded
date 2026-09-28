// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestCatalogGateCanceledAcquisition(t *testing.T) {
	for _, held := range []bool{false, true} {
		t.Run(map[bool]string{false: "available", true: "held"}[held], func(t *testing.T) {
			gate := newCatalogGate()
			if held {
				if err := gate.Acquire(t.Context()); err != nil {
					t.Fatal(err)
				}
			}

			ctx, cancel := context.WithCancel(t.Context())
			cancel()

			for range 100 {
				if err := gate.Acquire(ctx); !errors.Is(err, context.Canceled) {
					t.Fatalf("already canceled acquisition: %v", err)
				}
			}

			if held {
				gate.Release()
			}

			live, stop := context.WithTimeout(t.Context(), time.Second)
			defer stop()

			if err := gate.Acquire(live); err != nil {
				t.Fatalf("canceled acquisition consumed the gate: %v", err)
			}

			gate.Release()
		})
	}
}

func TestCatalogGateSerializesCallers(t *testing.T) {
	gate := newCatalogGate()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	// Deliberately non-atomic: the gate must protect each read/modify/write.
	count := 0

	for range 16 {
		wg.Go(func() {
			for range 100 {
				if err := gate.Acquire(ctx); err != nil {
					t.Errorf("acquire: %v", err)
					return
				}

				count++

				gate.Release()
			}
		})
	}

	wg.Wait()

	if count != 1600 {
		t.Fatalf("lost serialized updates: %d", count)
	}
}
