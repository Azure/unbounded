// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package dst

import (
	"bufio"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// EnvSweep selects a seed sweep, for example
// "tiny:1-300,small:1-150,medium:1-20". Each case runs twice and the traces
// are compared. Results are appended to EnvSweepOut, one line per case;
// cases already listed there are skipped, so an interrupted sweep resumes.
const (
	EnvSweep        = "CACHEMESH_DST_SWEEP"
	EnvSweepOut     = "CACHEMESH_DST_SWEEP_OUT"
	EnvSweepWorkers = "CACHEMESH_DST_SWEEP_WORKERS"
)

func sweepCase(size string, s uint64) (dstCase, bool) {
	switch size {
	case "tiny":
		return dstCase{name: fmt.Sprintf("tiny/%d", s), sc: shorten(Generate(s, SizeTiny), 240*sec, 180*sec)}, true
	case "small":
		return dstCase{name: fmt.Sprintf("small/%d", s), sc: shorten(Generate(s, SizeSmall), 180*sec, 180*sec)}, true
	case "medium":
		sc := shorten(Generate(s, SizeMedium), 60*sec, 150*sec)
		sc.PartitionBits = min(sc.PartitionBits, 16)

		return dstCase{name: fmt.Sprintf("medium/%d", s), sc: sc, prebuilt: true}, true
	case "restarts":
		return dstCase{name: fmt.Sprintf("restarts/%d", s), sc: GenerateRestarts(s, 8+int(s%40))}, true
	case "longrun":
		return dstCase{name: fmt.Sprintf("longrun/%d", s), sc: GenerateLongRun(s, 12, 2*3600*sec)}, true
	}

	return dstCase{}, false
}

func parseSweep(spec string) ([]dstCase, error) {
	var out []dstCase

	for part := range strings.SplitSeq(spec, ",") {
		size, rng, ok := strings.Cut(strings.TrimSpace(part), ":")
		if !ok {
			return nil, fmt.Errorf("sweep: %q", part)
		}

		lo, hi, _ := strings.Cut(rng, "-")
		if hi == "" {
			hi = lo
		}

		a, err1 := strconv.ParseUint(lo, 10, 64)
		b, err2 := strconv.ParseUint(hi, 10, 64)

		if err1 != nil || err2 != nil || b < a {
			return nil, fmt.Errorf("sweep: range %q", rng)
		}

		for s := a; s <= b; s++ {
			c, ok := sweepCase(size, s)
			if !ok {
				return nil, fmt.Errorf("sweep: size %q", size)
			}

			out = append(out, c)
		}
	}

	return out, nil
}

// TestDSTSweep runs a seed sweep selected by CACHEMESH_DST_SWEEP.
func TestDSTSweep(t *testing.T) {
	spec := os.Getenv(EnvSweep)
	if spec == "" {
		t.Skip("set " + EnvSweep)
	}

	cases, err := parseSweep(spec)
	if err != nil {
		t.Fatal(err)
	}

	done := map[string]bool{}

	path := os.Getenv(EnvSweepOut)
	if path == "" {
		t.Fatal("set " + EnvSweepOut)
	}

	if f, err := os.Open(path); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if name, _, ok := strings.Cut(sc.Text(), " "); ok {
				done[name] = true
			}
		}

		_ = f.Close()
	}

	out, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = out.Close() }()

	workers := runtime.GOMAXPROCS(0)
	if w, err := strconv.Atoi(os.Getenv(EnvSweepWorkers)); err == nil && w > 0 {
		workers = w
	}

	var (
		mu   sync.Mutex
		wg   sync.WaitGroup
		next = make(chan dstCase)
	)

	for range workers {
		wg.Go(func() {
			for c := range next {
				start := time.Now()
				r := runCase(c)
				r2 := runCase(c)

				status := "ok"

				switch {
				case r.err != nil:
					status = "FAIL " + strings.ReplaceAll(r.err.Error(), "\n", " | ")
				case r2.err != nil:
					status = "FAIL2 " + strings.ReplaceAll(r2.err.Error(), "\n", " | ")
				case r.trace != r2.trace || r.steps != r2.steps:
					status = "NONDETERMINISTIC"
				}

				mu.Lock()
				fmt.Fprintf(out, "%s %s wall=%v %s\n", c.name, status, time.Since(start).Round(time.Millisecond), strings.ReplaceAll(r.stats, "\n", " | "))
				mu.Unlock()
			}
		})
	}

	for _, c := range cases {
		if !done[c.name] {
			next <- c
		}
	}

	close(next)
	wg.Wait()
}
