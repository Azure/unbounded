// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package dst

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
)

func TestScenarioDeterministic(t *testing.T) {
	for _, sz := range []SizeClass{SizeTiny, SizeSmall, SizeMedium, SizeLarge, SizeHuge} {
		a, b := Generate(42, sz), Generate(42, sz)
		if !reflect.DeepEqual(a, b) {
			t.Fatalf("size %d not deterministic", sz)
		}

		if reflect.DeepEqual(a, Generate(43, sz)) {
			t.Fatal("seed ignored")
		}
	}

	if Generate(1, SizeLarge).Members != 10_000 || Generate(1, SizeHuge).Members != 100_000 {
		t.Fatal("size classes")
	}
}

func TestScenarioCoversEveryKind(t *testing.T) {
	seen := map[FaultKind]bool{}

	for seed := range uint64(200) {
		s := Generate(seed, SizeTiny)
		if len(s.Faults) == 0 {
			t.Fatal("no faults")
		}

		capacity := s.Members

		var prev int64

		for _, f := range s.Faults {
			seen[f.Kind] = true

			if f.Start < prev || f.Start < s.Warmup {
				t.Fatal("faults not sorted or before warmup")
			}

			prev = f.Start

			if f.End() > s.Duration-s.Settle {
				t.Fatal("fault overlaps settle period")
			}

			if f.Kind == MassJoin {
				capacity += f.Count
			}
		}

		if capacity != s.Capacity || s.Settle <= 0 {
			t.Fatal("capacity/settle")
		}
	}

	for _, k := range AllFaultKinds() {
		if !seen[k] {
			t.Fatalf("kind %s never generated", k)
		}

		s := GenerateKinds(7, SizeSmall, k)
		if got := s.Kinds(); len(got) != 1 || got[0] != k {
			t.Fatalf("GenerateKinds(%s) = %v", k, got)
		}

		if k.String() == "unknown" {
			t.Fatal("name")
		}
	}

	if len(AllFaultKinds()) != 17 || FaultKind(0).String() != "unknown" {
		t.Fatal("kinds")
	}

	s := Generate(3, SizeSmall)
	if !strings.Contains(s.String(), "seed=3") {
		t.Fatal(s.String())
	}
}

func TestKeyGen(t *testing.T) {
	g := NewKeyGen(NewRand(1, "w"), 1000)
	counts := map[KeyKind]int{}
	ranks := make([]int, 1000)

	for range 20_000 {
		k, kind, rank := g.Next()
		counts[kind]++
		ranks[rank]++

		switch kind {
		case KeyBlob:
			if len(k) != 32 {
				t.Fatal("blob len")
			}
		case KeyStripe:
			if len(k) != 36 {
				t.Fatal("stripe len")
			}
		case KeyMeta:
			if !bytes.HasPrefix(k, []byte("repo-")) || !bytes.Contains(k, []byte(":tag-")) {
				t.Fatalf("meta %q", k)
			}
		}
	}

	if counts[KeyBlob] == 0 || counts[KeyStripe] == 0 || counts[KeyMeta] == 0 {
		t.Fatal(counts)
	}

	if ranks[0] < ranks[100]*5 {
		t.Fatal("not skewed")
	}

	a := append([]byte(nil), g.Blob(5)...)
	if !bytes.Equal(a, g.Blob(5)) || bytes.Equal(a, g.Blob(6)) || !bytes.Equal(g.Stripe(5, 1)[:32], a) {
		t.Fatal("digests")
	}

	if n := testing.AllocsPerRun(1000, func() { g.Next() }); n != 0 {
		t.Fatalf("Next allocates %v", n)
	}

	h := NewKeyGen(NewRand(1, "w"), 1000)
	g2 := NewKeyGen(NewRand(1, "w"), 1000)

	for range 100 {
		x, _, _ := h.Next()
		y, _, _ := g2.Next()

		if !bytes.Equal(x, y) {
			t.Fatal("nondeterministic")
		}
	}
}

func TestSeedsFromEnv(t *testing.T) {
	def := []uint64{1, 2}

	t.Setenv(EnvSeed, "")
	t.Setenv(EnvSeeds, "")

	if got, err := SeedsFromEnv(def); err != nil || !reflect.DeepEqual(got, def) {
		t.Fatal(got, err)
	}

	t.Setenv(EnvSeeds, "3")

	if got, err := SeedsFromEnv(def); err != nil || len(got) != 5 || got[0] != 1 {
		t.Fatal(got, err)
	}

	t.Setenv(EnvSeeds, "7, 0x10,")

	if got, err := SeedsFromEnv(def); err != nil || !reflect.DeepEqual(got, []uint64{7, 16}) {
		t.Fatal(got, err)
	}

	t.Setenv(EnvSeeds, "x,1")

	if _, err := SeedsFromEnv(def); err == nil {
		t.Fatal("bad list accepted")
	}

	t.Setenv(EnvSeed, "99")

	if got, err := SeedsFromEnv(def); err != nil || !reflect.DeepEqual(got, []uint64{99}) {
		t.Fatal(got, err)
	}

	t.Setenv(EnvSeed, "nope")

	if _, err := SeedsFromEnv(def); err == nil {
		t.Fatal("bad seed accepted")
	}

	if def[0] != 1 || def[1] != 2 {
		t.Fatal("defaults modified")
	}

	t.Setenv(EnvLarge, "1")

	if !Large() {
		t.Fatal("large")
	}

	if got := ReplayCommand(5); got != "CACHEMESH_DST_SEED=5 go test ./internal/cachemesh/internal/dst -run TestDST -count=1 -v" {
		t.Fatal(got)
	}
}
