// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package app

import "testing"

func TestZipfConfigValidate(t *testing.T) {
	cases := []struct {
		name    string
		cfg     zipfConfig
		wantErr bool
	}{
		{name: "default ok", cfg: defaultZipfConfig()},
		{name: "s too low", cfg: zipfConfig{s: 1.0, v: 1}, wantErr: true},
		{name: "v too low", cfg: zipfConfig{s: 1.1, v: 0.5}, wantErr: true},
		{name: "uniform skips zipf checks", cfg: zipfConfig{s: 0, v: 0, uniform: true}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.validate()
			if tc.wantErr != (err != nil) {
				t.Fatalf("validate() err = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}

func TestSelectorInBounds(t *testing.T) {
	const count = 500

	cfg := defaultZipfConfig()

	sel, err := cfg.newSelector(count, 0)
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 100000; i++ {
		v := sel.pick()
		if v < 0 || v >= count {
			t.Fatalf("pick returned out-of-range index %d", v)
		}
	}
}

func TestSelectorUniformInBounds(t *testing.T) {
	const count = 500

	cfg := defaultZipfConfig()
	cfg.uniform = true

	sel, err := cfg.newSelector(count, 0)
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 100000; i++ {
		v := sel.pick()
		if v < 0 || v >= count {
			t.Fatalf("pick returned out-of-range index %d", v)
		}
	}
}

func TestSelectorDeterministic(t *testing.T) {
	cfg := defaultZipfConfig()

	s1, _ := cfg.newSelector(1000, 3)
	s2, _ := cfg.newSelector(1000, 3)

	for i := 0; i < 1000; i++ {
		if a, b := s1.pick(), s2.pick(); a != b {
			t.Fatalf("iteration %d: %d != %d (not deterministic)", i, a, b)
		}
	}
}

func TestSelectorZeroCount(t *testing.T) {
	cfg := defaultZipfConfig()
	if _, err := cfg.newSelector(0, 0); err == nil {
		t.Fatal("expected error for zero count")
	}
}

func TestPermutationIsBijection(t *testing.T) {
	for _, n := range []uint64{1, 2, 3, 7, 16, 17, 100, 1000} {
		perm := newPermutation(n, 0x1234)
		seen := make(map[uint64]bool, n)

		for x := uint64(0); x < n; x++ {
			y := perm.permute(x)
			if y >= n {
				t.Fatalf("n=%d: permute(%d)=%d out of range", n, x, y)
			}

			if seen[y] {
				t.Fatalf("n=%d: permute produced duplicate %d", n, y)
			}

			seen[y] = true
		}

		if uint64(len(seen)) != n {
			t.Fatalf("n=%d: covered %d distinct values", n, len(seen))
		}
	}
}

func TestPermutationDeterministic(t *testing.T) {
	p1 := newPermutation(1000, 42)
	p2 := newPermutation(1000, 42)
	p3 := newPermutation(1000, 43)

	same := true

	for x := uint64(0); x < 1000; x++ {
		if p1.permute(x) != p2.permute(x) {
			t.Fatalf("same seed disagreed at %d", x)
		}

		if p1.permute(x) != p3.permute(x) {
			same = false
		}
	}

	if same {
		t.Fatal("different seeds produced identical permutation")
	}
}

func TestSelectorSkewFavorsHotKeys(t *testing.T) {
	// With shuffle disabled, a high skew should concentrate draws.
	cfg := defaultZipfConfig()
	cfg.s = 2.0
	cfg.shuffleSeed = 0

	sel, err := cfg.newSelector(10000, 0)
	if err != nil {
		t.Fatal(err)
	}

	counts := make(map[int64]int)

	const n = 100000
	for i := 0; i < n; i++ {
		counts[sel.pick()]++
	}

	// Index 0 is the hottest under Zipf without shuffle.
	if counts[0] < n/100 {
		t.Fatalf("expected index 0 to be hot, got %d of %d draws", counts[0], n)
	}
}
