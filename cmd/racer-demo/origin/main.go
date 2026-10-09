// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/sha256"
	"io"
	"log"
	"math/rand/v2"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/Azure/unbounded/pkg/racersdk"
)

const (
	docSize        = 42 << 20 // 42MB
	reportInterval = 5 * time.Second
)

var words = [...]string{
	"Clippy", "BSOD", "Zune", "Steve Ballmer", "ctrl-alt-del", "Can You See My Screen", "Update Tuesday", "sev0",
}

func main() {
	ctx := context.TODO()

	stats := &originStats{}
	go stats.report(ctx)

	conf := racersdk.OriginConfig{Cache: "racer-demo"}

	err := racersdk.ServeOrigin(ctx, conf, func(ctx context.Context, r racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
		doc, err := strconv.ParseUint(r.Metadata, 10, 64)
		if err != nil || r.Key != sha256.Sum256([]byte("racer-demo/doc/"+strconv.FormatUint(doc, 10))) {
			return racersdk.Metadata{}, nil, racersdk.ErrNotFound
		}

		metadata := racersdk.Metadata{
			Size:        docSize,
			ETag:        `"v1"`,
			ContentType: "text/plain; charset=utf-8",
			ExpiresAt:   time.Now().Add(time.Hour),
		}
		if r.Head {
			// Only return metadata for HEAD requests
			return metadata, nil, nil
		}

		if r.Offset >= docSize || r.Length == 0 {
			return metadata, nil, nil
		}

		start := r.Offset
		end := start + min(r.Length, docSize-start)

		return metadata, io.NopCloser(&docReader{ctx: ctx, doc: doc, offset: start, end: end, stats: stats}), nil
	})
	if err != nil {
		panic(err)
	}
}

type docReader struct {
	ctx         context.Context
	doc         uint64
	offset, end int64
	stats       *originStats
}

func (r *docReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}

	if r.offset == r.end {
		return 0, io.EOF
	}

	n := 0

	for len(p) > 0 && r.offset < r.end {
		var line [64]byte
		for i := range line {
			line[i] = ' '
		}

		line[len(line)-1] = '\n'

		rng := rand.NewPCG(r.doc, uint64(r.offset/64))
		for pos := 0; pos < len(line)-1; {
			word := words[rng.Uint64()%uint64(len(words))]
			if pos+len(word) > len(line)-1 {
				break
			}

			pos += copy(line[pos:], word) + 1
		}

		count := copy(p, line[r.offset%64:min(64, r.offset%64+r.end-r.offset)])
		r.offset += int64(count)
		n += count
		p = p[count:]
	}

	r.stats.bytes.Add(int64(n))

	if r.offset == r.end {
		r.stats.pages.Add(1)
	}

	return n, nil
}

type originStats struct {
	bytes atomic.Int64
	pages atomic.Int64
}

func (s *originStats) report(ctx context.Context) {
	ticker := time.NewTicker(reportInterval)
	defer ticker.Stop()

	last := time.Now()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := time.Now()
			bytes := s.bytes.Swap(0)
			log.Printf("origin pages=%d throughput=%.2f Gbit/s", s.pages.Swap(0), float64(bytes)*8/1e9/now.Sub(last).Seconds())
			last = now
		}
	}
}
