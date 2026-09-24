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

	"github.com/Azure/unbounded/pkg/racersdk"
)

// All replicas serve the same immutable dataset, without materializing it.
type dataset struct {
	prefix      string
	size, count int64
	ttl         time.Duration
	checksums   []datasetChecksum
	ctx         context.Context
	cancel      context.CancelFunc
	jobs        chan int64
	stopped     chan struct{}
	hash        func(context.Context, string) ([32]byte, error)
}

type datasetChecksum struct {
	once sync.Once
	done chan struct{}
	sum  [32]byte
	err  error
}

func newDataset(ctx context.Context, c config) *dataset {
	ctx, cancel := context.WithCancel(ctx)
	d := &dataset{
		prefix: fmt.Sprintf("/loadgen/v1/%d/%d/", c.footprint, c.objectSize),
		size:   c.objectSize, count: c.footprint / c.objectSize, ttl: c.ttl,
		checksums: make([]datasetChecksum, c.footprint/c.objectSize),
		ctx:       ctx, cancel: cancel,
		jobs: make(chan int64, c.footprint/c.objectSize), stopped: make(chan struct{}),
	}

	d.hash = d.checksum
	go d.publish()

	return d
}

// Close cancels publication and joins the single worker, including on setup failure.
func (d *dataset) Close() {
	d.cancel()
	<-d.stopped
}

func (d *dataset) publish() {
	defer close(d.stopped)

	for {
		var id int64
		select {
		case <-d.ctx.Done():
			return
		case id = <-d.jobs:
		}

		if d.ctx.Err() != nil {
			return
		}

		entry := &d.checksums[id]
		entry.sum, entry.err = d.hash(d.ctx, d.target(int(id)))
		// Closing done publishes the immutable result to all waiters.
		close(entry.done)
	}
}

func (d *dataset) target(id int) string { return d.prefix + strconv.Itoa(id) }

func (d *dataset) Stat(ctx context.Context, target string, _ []byte) (racersdk.Metadata, error) {
	if err := ctx.Err(); err != nil {
		return racersdk.Metadata{}, err
	}

	suffix, ok := strings.CutPrefix(target, d.prefix)

	id, err := strconv.ParseInt(suffix, 10, 64)
	if !ok || err != nil || id < 0 || id >= d.count || suffix != strconv.FormatInt(id, 10) {
		return racersdk.Metadata{}, fs.ErrNotExist
	}

	entry := &d.checksums[id]
	entry.once.Do(func() {
		if d.ctx.Err() != nil {
			return
		}

		// Each object is queued at most once, so the dataset-sized queue cannot
		// block. Requests never own publication or spawn hashing goroutines.
		entry.done = make(chan struct{})

		d.jobs <- id
	})

	select {
	case <-ctx.Done():
	case <-d.ctx.Done():
	case <-entry.done:
	}

	if err := ctx.Err(); err != nil {
		return racersdk.Metadata{}, err
	}

	if err := d.ctx.Err(); err != nil {
		return racersdk.Metadata{}, err
	}

	if entry.err != nil {
		return racersdk.Metadata{}, entry.err
	}

	ttl := d.ttl

	return racersdk.Metadata{Size: d.size, ETag: fmt.Sprintf(`"%x"`, entry.sum), TTL: &ttl}, nil
}

func (d *dataset) checksum(ctx context.Context, target string) ([32]byte, error) {
	return checksum(ctx, d.source(target), d.size)
}

func checksum(ctx context.Context, source io.ReaderAt, size int64) ([32]byte, error) {
	// Hash the actual synthetic bytes once per object, using bounded scratch.
	// The target hash seeds the generator; it is not a content checksum.
	h := sha256.New()
	buf := make([]byte, 32*1024)

	for off := int64(0); off < size; {
		if err := ctx.Err(); err != nil {
			return [32]byte{}, err
		}

		n, err := source.ReadAt(buf, off)
		if err != nil && err != io.EOF {
			return [32]byte{}, err
		}

		h.Write(buf[:n])
		off += int64(n)
	}

	return [32]byte(h.Sum(nil)), nil
}

func (d *dataset) Open(ctx context.Context, target, etag string, _ []byte) (racersdk.Source, error) {
	m, err := d.Stat(ctx, target, nil)
	if err != nil {
		return nil, err
	}

	if etag != m.ETag {
		return nil, racersdk.ErrVersionChanged
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
