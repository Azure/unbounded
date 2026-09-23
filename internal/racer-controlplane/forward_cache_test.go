// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestForwardCacheExactStateAndIsolation(t *testing.T) {
	_, r, _ := forwardFixture(t)

	first, err := r.forwardHistory("default")
	if err != nil || len(first) != 1 {
		t.Fatal(first, err)
	}

	cache := r.forwards

	second, err := r.forwardHistory("default")
	if err != nil || r.forwards != cache || !reflect.DeepEqual(first, second) {
		t.Fatal("unchanged durable state was not reused", err)
	}

	first[0].Boot = strings.Repeat("ab", 32)
	first[0].Ref.Node = strings.Repeat("ff", 32)
	first = append(first, first[0])

	third, err := r.forwardHistory("default")
	if err != nil || !reflect.DeepEqual(second, third) || len(first) != 2 {
		t.Fatal("caller mutation leaked into cache", err)
	}

	// Even when bytes happen to agree, replacing the durable object or its RV
	// revalidates instead of carrying validation across distinct readback state.
	r.pointer = r.pointer.DeepCopy()
	if _, err := r.forwardHistory("default"); err != nil || r.forwards == cache {
		t.Fatal("new pointer reused old cache", err)
	}

	cache = r.forwards

	r.pointer.ResourceVersion += "-changed"
	if _, err := r.forwardHistory("default"); err != nil || r.forwards == cache {
		t.Fatal("changed resourceVersion reused old cache", err)
	}

	r.pointer.Data["forwards"] = "[]"
	if ds, err := r.forwardHistory("default"); err != nil || len(ds) != 0 {
		t.Fatal("changed bytes reused old cache", err)
	}
}

func TestForwardCacheRejectsChangedValidationInputs(t *testing.T) {
	for _, change := range []string{"corrupt", "universe", "revision", "invalid", "entry-budget", "byte-budget", "payload-budget"} {
		t.Run(change, func(t *testing.T) {
			_, r, _ := forwardFixture(t)

			ds, err := r.forwardHistory("default")
			if err != nil {
				t.Fatal(err)
			}

			raw := r.pointer.Data["forwards"]
			universe := "default"

			switch change {
			case "corrupt":
				r.pointer.Data["forwards"] = "{"
			case "universe":
				universe = "another"
			case "revision":
				r.revision = 0
			case "invalid":
				r.invalidate()
			case "entry-budget":
				full := make([]forwardDecision, catchupLimit+1)
				for i := range full {
					full[i] = ds[0]
				}

				data, err := json.Marshal(full)
				if err != nil {
					t.Fatal(err)
				}

				r.pointer.Data["forwards"] = string(data)
			case "byte-budget":
				r.pointer.Data["forwards"] = strings.Repeat(" ", forwardBytes+1)
			case "payload-budget":
				ds[0].Ref.Size = forwardSnapshotBytes + 1

				data, err := json.Marshal(ds)
				if err != nil {
					t.Fatal(err)
				}

				r.pointer.Data["forwards"] = string(data)
			}

			if _, err := r.forwardHistory(universe); err == nil || r.forwards != nil {
				t.Fatal("changed invalid state accepted or cached", err)
			}

			r.pointer.Data["forwards"] = raw

			r.revision = 2
			if change == "invalid" {
				if _, err := r.forwardHistory("default"); err == nil {
					t.Fatal("invalid handle reused restored bytes")
				}
			} else if ds, err := r.forwardHistory("default"); err != nil || len(ds) != 1 {
				t.Fatal("failed validation poisoned later valid state", err)
			}
		})
	}
}

func TestForwardCacheWritesAndUncertainReadback(t *testing.T) {
	for _, fault := range []string{"", "lost", "lost-read", "no-commit", "history-conflict"} {
		t.Run("fault="+fault, func(t *testing.T) {
			ctx := context.Background()
			f, r, digest := forwardFixture(t)

			original, err := r.forwardHistory("default")
			if err != nil {
				t.Fatal(err)
			}

			cache := r.forwards
			f.api.fault = fault
			boot := strings.Repeat("ab", 32)

			_, _, _, err = f.s.forward(ctx, r, "default", f.node, "pod-uid", boot, digest, digest, 0)
			if fault != "" {
				if err == nil || !r.invalid || r.forwards != nil {
					t.Fatal("uncertain write did not invalidate cache", err)
				}

				if _, err := r.forwardHistory("default"); err == nil {
					t.Fatal("invalid rollout served cached history")
				}
			} else if err != nil {
				t.Fatal(err)
			}

			if !reflect.DeepEqual(cache.entries, original) {
				t.Fatal("grant mutated old cache before persistence")
			}

			for range 4 {
				r, err = f.s.rolloutFor(ctx, f.index)
				if err == nil {
					break
				}
			}

			if err != nil {
				t.Fatal(err)
			}

			got, err := r.forwardHistory("default")

			want := 1
			if fault == "" || fault == "lost" || fault == "lost-read" {
				want = 2
			}

			if err != nil || len(got) != want {
				t.Fatal("cache disagrees with write outcome", len(got), want, err)
			}

			if _, _, _, err := f.s.forward(ctx, r, "default", f.node, "pod-uid", boot, digest, digest, 0); err != nil {
				t.Fatal(err)
			}

			if err := f.s.collectForwards(ctx, r, "default", f.node, "pod-uid", boot); err != nil {
				t.Fatal(err)
			}

			got, err = r.forwardHistory("default")
			if err != nil || !reflect.DeepEqual(got, original) {
				t.Fatal("collection lost wildcard or reused bound cache", err)
			}
			// A metadata cache hit must not bypass immutable payload verification.
			name := forwardChunkName(stateName("default"), got[0].Ref, 0)

			part := r.pointer.DeepCopy()
			if err := f.api.Get(ctx, client.ObjectKey{Namespace: "state", Name: name}, part); err != nil {
				t.Fatal(err)
			}

			if err := f.api.Delete(ctx, part); err != nil {
				t.Fatal(err)
			}

			if _, _, _, err := f.s.forward(ctx, r, "default", f.node, "pod-uid", boot, digest, digest, 0); err == nil {
				t.Fatal("cached metadata bypassed missing payload")
			}
		})
	}
}

func TestForwardCacheInvalidatedByOtherRolloutWrites(t *testing.T) {
	for _, operation := range []string{"phase", "removal"} {
		t.Run(operation, func(t *testing.T) {
			ctx := context.Background()

			f, r, _ := forwardFixture(t)
			if _, err := r.forwardHistory("default"); err != nil {
				t.Fatal(err)
			}

			f.api.fault = "lost"

			var err error
			if operation == "phase" {
				err = f.s.persistPhase(ctx, "default", r, 2)
			} else {
				ds, loadErr := removalHistory(r.pointer.Data["removals"], "default", r.revision)
				if loadErr != nil {
					t.Fatal(loadErr)
				}

				err = f.s.saveRemovals(ctx, r, ds)
			}

			if err == nil || !r.invalid || r.forwards != nil {
				t.Fatal("uncertain non-forward write retained validation", err)
			}

			if _, err := r.forwardHistory("default"); err == nil {
				t.Fatal("invalid rollout authorized cached history")
			}
			// An uncached read returning corrupt history must not inherit validation
			// from the previous handle, even though its forward write was unchanged.
			cm := f.durable(t)

			cm.Data["forwards"] = "{"
			if err := f.api.Client.Update(ctx, cm); err != nil {
				t.Fatal(err)
			}

			if _, err := f.s.rolloutFor(ctx, f.index); err == nil {
				t.Fatal("corrupt durable readback reused old cache")
			}
		})
	}
}
