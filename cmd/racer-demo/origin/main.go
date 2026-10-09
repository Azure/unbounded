// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/sha256"
	"io"
	"math/rand/v2"
	"time"

	"github.com/Azure/unbounded/pkg/racersdk"
)

const docSize = 42 << 20 // 42 MiB

var words = [...]string{
	"Clippy", "BSOD", "Zune", "Steve Ballmer", "ctrl-alt-del", "Can You See My Screen", "Update Tuesday", "sev0",
}

func main() {
	ctx := context.TODO()

	conf := racersdk.OriginConfig{Cache: "racer-demo"}

	err := racersdk.ServeOrigin(ctx, conf, serveDocument)
	if err != nil {
		panic(err)
	}
}

func serveDocument(ctx context.Context, r racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
	if r.Key != sha256.Sum256([]byte("0")) {
		return racersdk.Metadata{}, nil, racersdk.ErrNotFound
	}

	metadata := racersdk.Metadata{
		Size:        docSize,
		ETag:        `"v1"`,
		ContentType: "text/plain; charset=utf-8",
		ExpiresAt:   time.Now().Add(time.Hour),
	}
	if r.Head {
		return metadata, nil, nil
	}

	if r.Offset >= docSize || r.Length == 0 {
		return metadata, nil, nil
	}

	start := r.Offset
	end := start + min(r.Length, docSize-start)

	return metadata, io.NopCloser(&docReader{ctx: ctx, offset: start, end: end}), nil
}

type docReader struct {
	ctx         context.Context
	offset, end int64
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

		rng := rand.NewPCG(0, uint64(r.offset/64))
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

	return n, nil
}
