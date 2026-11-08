// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package dst

import (
	"fmt"
	"math/rand/v2"
	"os"
	"strconv"
	"strings"
)

// Environment variables that select seeds and run size.
const (
	// EnvSeeds is either a count of extra random seeds to run in addition to
	// the defaults, or a comma-separated list of seeds to run instead.
	EnvSeeds = "CACHEMESH_DST_SEEDS"
	// EnvSeed replays exactly one seed.
	EnvSeed = "CACHEMESH_DST_SEED"
	// EnvLarge enables the 100k-member runs when set to 1.
	EnvLarge = "CACHEMESH_DST_LARGE"
)

// SeedsFromEnv returns the seeds to run. CACHEMESH_DST_SEED wins and
// returns that single seed. Otherwise CACHEMESH_DST_SEEDS is a count N
// (defaults plus N fresh random seeds) or a comma list (exactly those
// seeds). With neither set the defaults are returned. Fresh seeds come from
// the process-wide random source; they are tooling input, not simulation
// state, and each is printed by the suite so it can be replayed.
func SeedsFromEnv(defaults []uint64) ([]uint64, error) {
	if v := strings.TrimSpace(os.Getenv(EnvSeed)); v != "" {
		s, err := parseSeed(v)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", EnvSeed, err)
		}

		return []uint64{s}, nil
	}

	out := append([]uint64(nil), defaults...)

	v := strings.TrimSpace(os.Getenv(EnvSeeds))
	if v == "" {
		return out, nil
	}

	if !strings.Contains(v, ",") {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 && n < 1<<20 {
			for range n {
				out = append(out, rand.Uint64()) //nolint:gosec // seed selection, not simulation state
			}

			return out, nil
		}
	}

	out = out[:0]

	for f := range strings.SplitSeq(v, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}

		s, err := parseSeed(f)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", EnvSeeds, err)
		}

		out = append(out, s)
	}

	return out, nil
}

func parseSeed(s string) (uint64, error) {
	return strconv.ParseUint(s, 0, 64)
}

// Large reports whether CACHEMESH_DST_LARGE=1.
func Large() bool { return os.Getenv(EnvLarge) == "1" }

// ReplayCommand returns the shell command that replays seed.
func ReplayCommand(seed uint64) string {
	return fmt.Sprintf("%s=%d go test ./internal/cachemesh/internal/dst -run TestDST -count=1 -v", EnvSeed, seed)
}
