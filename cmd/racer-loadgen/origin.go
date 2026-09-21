// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"io/fs"
	"strconv"
	"strings"
	"sync"
	"time"

	racer "github.com/Azure/unbounded/pkg/racer"
)

// All replicas serve the same immutable dataset, without materializing it.
type dataset struct {
	prefix      string
	size, count int64
	ttl         time.Duration
	checksums   []datasetChecksum
}

type datasetChecksum struct {
	mu    sync.Mutex
	ready bool
	sum   [32]byte
}

func newDataset(c config) *dataset {
	return &dataset{
		prefix: fmt.Sprintf("/loadgen/v1/%d/%d/", c.footprint, c.objectSize),
		size:   c.objectSize, count: c.footprint / c.objectSize, ttl: c.ttl,
		checksums: make([]datasetChecksum, c.footprint/c.objectSize),
	}
}

func (d *dataset) target(id int) string { return d.prefix + strconv.Itoa(id) }

func (d *dataset) Stat(ctx context.Context, target string) (racer.Metadata, error) {
	if err := ctx.Err(); err != nil {
		return racer.Metadata{}, err
	}

	suffix, ok := strings.CutPrefix(target, d.prefix)

	id, err := strconv.ParseInt(suffix, 10, 64)
	if !ok || err != nil || id < 0 || id >= d.count || suffix != strconv.FormatInt(id, 10) {
		return racer.Metadata{}, fs.ErrNotExist
	}

	ttl := d.ttl
	entry := &d.checksums[id]
	entry.mu.Lock()
	defer entry.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return racer.Metadata{}, err
	}

	if !entry.ready {
		// Hash the actual synthetic bytes once per object, using bounded scratch.
		// The target hash seeds the generator; it is not a content checksum.
		source := d.source(target)
		h := sha256.New()
		buf := make([]byte, 32*1024)

		for off := int64(0); off < d.size; {
			if err := ctx.Err(); err != nil {
				return racer.Metadata{}, err
			}

			n, err := source.ReadAt(buf, off)
			if err != nil && err != io.EOF {
				return racer.Metadata{}, err
			}

			h.Write(buf[:n])
			off += int64(n)
		}

		copy(entry.sum[:], h.Sum(nil))
		entry.ready = true
	}

	return racer.Metadata{Size: d.size, ETag: fmt.Sprintf(`"%x"`, entry.sum), TTL: &ttl}, nil
}

func (d *dataset) Open(ctx context.Context, target, etag string) (racer.Source, error) {
	m, err := d.Stat(ctx, target)
	if err != nil {
		return nil, err
	}

	if etag != m.ETag {
		return nil, racer.ErrVersionChanged
	}

	return d.source(target), nil
}

func (d *dataset) source(target string) syntheticSource {
	hash := sha256.Sum256([]byte(target))
	return syntheticSource{size: d.size, key: binary.LittleEndian.Uint64(hash[:])}
}

type syntheticSource struct {
	size int64
	key  uint64
}

func (s syntheticSource) Close() error { return nil }

// SplitMix64's mixing function gives inexpensive, non-repeating 64-bit words.
// This is synthetic benchmark data, not a cryptographic random stream.
func mix(x uint64) uint64 {
	x = (x ^ (x >> 30)) * 0xbf58476d1ce4e5b9
	x = (x ^ (x >> 27)) * 0x94d049bb133111eb

	return x ^ (x >> 31)
}

func (s syntheticSource) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, fmt.Errorf("negative offset")
	}

	if len(p) == 0 {
		return 0, nil
	}

	if off >= s.size {
		return 0, io.EOF
	}

	n := int(min(int64(len(p)), s.size-off))

	written := 0
	for written < n {
		pos := off + int64(written)

		word := mix(s.key + uint64(pos/8)*0x9e3779b97f4a7c15)
		if pos%8 == 0 && n-written >= 8 {
			binary.LittleEndian.PutUint64(p[written:], word)
			written += 8
		} else {
			for b := pos % 8; b < 8 && written < n; b++ {
				p[written] = byte(word >> (8 * b))
				written++
			}
		}
	}

	if n < len(p) {
		return n, io.EOF
	}

	return n, nil
}
