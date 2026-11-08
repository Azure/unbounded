// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package viewtest

import (
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/cachemesh/internal/view"
)

func TestBuildSmall(t *testing.T) {
	for _, n := range []int{0, 1, 2, 5, 100, 1000} {
		v := Build(t, n, Options{PartitionBits: 12, Joining: n / 10, Draining: n / 20})
		if err := view.Validate(v); err != nil {
			t.Fatalf("n=%d: %v", n, err)
		}

		if v.Len() != n {
			t.Fatalf("n=%d: Len %d", n, v.Len())
		}

		for i := range 5 {
			d := Delta(t, v, Churn{Admit: 3, Remove: min(2, n/2), Moves: 64, Epoch: i%2 == 0})

			nv, err := view.Apply(v, d)

			if n == 0 {
				continue
			}

			if err != nil {
				t.Fatalf("n=%d step %d: %v", n, i, err)
			}

			if err := view.Validate(nv); err != nil {
				t.Fatalf("n=%d step %d: %v", n, i, err)
			}

			v = nv
		}
	}
}

func TestBuildLarge(t *testing.T) {
	if testing.Short() {
		t.Skip("large")
	}

	start := time.Now()
	v := Build(t, 100_000, Options{})
	t.Logf("built 100k/2^20 in %v", time.Since(start))

	if v.Len() != 100_000 || v.PartitionBits() != 20 {
		t.Fatal("wrong size")
	}
}
