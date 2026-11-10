// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"golang.org/x/sys/unix"

	"github.com/Azure/unbounded/pkg/racersdk"
)

// Delivery modes for --delivery.
const (
	deliveryStream   = "stream"
	deliveryHostRead = "host-read"
)

// hostReader is the subset of the SDK client a host-read pull needs.
type hostReader interface {
	Stat(ctx context.Context, request racersdk.Request) (racersdk.Metadata, error)
	HostRead(ctx context.Context, request racersdk.Request, file *os.File, options racersdk.HostReadOptions) (racersdk.HostReadResult, error)
}

var _ hostReader = (*racersdk.Client)(nil)

// memfd is one reusable destination. Its pages are allocated by the loadgen
// with fallocate, so they are charged to this process's memory cgroup rather
// than the dataplane's, and stay populated across pulls. It is sealed against
// shrinking, so Racer may decrypt into a mapping of it.
type memfd struct {
	file *os.File
	size int64
	// mapping is a read-only shared view used only for verification.
	mapping []byte
}

// grow makes the file at least size bytes, allocating its pages.
func (m *memfd) grow(size int64) error {
	if size <= m.size {
		return nil
	}

	if err := m.unmap(); err != nil {
		return err
	}

	if err := m.file.Truncate(size); err != nil {
		return fmt.Errorf("resize memfd: %w", err)
	}

	if err := unix.Fallocate(int(m.file.Fd()), 0, 0, size); err != nil {
		return fmt.Errorf("allocate memfd: %w", err)
	}

	m.size = size

	return nil
}

// view returns the first n bytes of the file through a shared mapping.
func (m *memfd) view(n int64) ([]byte, error) {
	if n > m.size {
		return nil, fmt.Errorf("view %d bytes of a %d byte memfd", n, m.size)
	}

	if n == 0 {
		return nil, nil
	}

	if m.mapping == nil {
		mapping, err := unix.Mmap(int(m.file.Fd()), 0, int(m.size), unix.PROT_READ, unix.MAP_SHARED)
		if err != nil {
			return nil, fmt.Errorf("map memfd: %w", err)
		}

		m.mapping = mapping
	}

	return m.mapping[:n], nil
}

func (m *memfd) unmap() error {
	if m.mapping == nil {
		return nil
	}

	err := unix.Munmap(m.mapping)
	m.mapping = nil

	return err
}

func (m *memfd) close() error {
	return errors.Join(m.unmap(), m.file.Close())
}

// memfdPool hands each concurrent pull its own memfd and reuses them, so a
// steady workload allocates no destination memory per pull. The pool holds
// at most one memfd per concurrent pull.
type memfdPool struct {
	mu     sync.Mutex
	free   []*memfd
	all    []*memfd
	closed bool
}

func (p *memfdPool) get(size int64) (*memfd, error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, errors.New("memfd pool closed")
	}

	var m *memfd
	if n := len(p.free); n != 0 {
		m = p.free[n-1]
		p.free = p.free[:n-1]
	}
	p.mu.Unlock()

	if m == nil {
		fd, err := unix.MemfdCreate("racer-loadgen", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING)
		if err != nil {
			return nil, fmt.Errorf("create memfd: %w", err)
		}

		// A memfd that cannot shrink lets Racer map it and decrypt pages
		// straight into it. The file can still grow.
		if _, err := unix.FcntlInt(uintptr(fd), unix.F_ADD_SEALS, unix.F_SEAL_SHRINK); err != nil {
			if cerr := unix.Close(fd); cerr != nil {
				return nil, fmt.Errorf("seal memfd: %w (close: %w)", err, cerr)
			}

			return nil, fmt.Errorf("seal memfd: %w", err)
		}

		m = &memfd{file: os.NewFile(uintptr(fd), "racer-loadgen-memfd")}

		p.mu.Lock()
		p.all = append(p.all, m)
		p.mu.Unlock()
	}

	if err := m.grow(size); err != nil {
		p.put(m)
		return nil, err
	}

	return m, nil
}

func (p *memfdPool) put(m *memfd) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.free = append(p.free, m)
}

// close releases every memfd. Pulls must have finished.
func (p *memfdPool) close() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.closed = true

	var errs []error
	for _, m := range p.all {
		errs = append(errs, m.close())
	}

	p.all, p.free = nil, nil

	return errors.Join(errs...)
}

// hostReadBody is a pull whose bytes Racer writes straight into a memfd. Read
// is unsupported; readBody runs the transfer with deliver and verifies by
// hashing the memfd.
type hostReadBody struct {
	ranged *rangedBody
	pool   *memfdPool
	dest   *memfd
	size   int64
	once   sync.Once
}

func (b *hostReadBody) Read([]byte) (int, error) {
	return 0, errors.New("host-read body supports only deliver")
}

func (b *hostReadBody) Close() error {
	b.once.Do(func() { b.pool.put(b.dest) })
	return nil
}

// deliver runs every host-read and returns the bytes Racer confirmed.
func (b *hostReadBody) deliver() (int64, error) {
	return b.ranged.WriteTo(io.Discard)
}

// digest hashes the delivered bytes in place.
func (b *hostReadBody) digest() (string, error) {
	view, err := b.dest.view(b.size)
	if err != nil {
		return "", err
	}

	return fmt.Sprintf("sha256:%x", sha256.Sum256(view)), nil
}

// readHostBody runs a host-read pull. Received bytes are counted per page as
// Racer confirms them.
func (p *puller) readHostBody(body *hostReadBody) (int64, string, error) {
	n, err := body.deliver()
	if err != nil || !p.opts.Verify {
		return n, "", err
	}

	actual, err := body.digest()

	return n, actual, err
}

// hostReadRange performs one host-read of [offset, offset+length) into the
// memfd at the same offset. It reports confirmed bytes through the returned
// WriterTo, which ignores its writer, so rangedBody can resume a failed range
// at its first unconfirmed byte. Pages arrive in order, so the confirmed
// bytes are a prefix of the range.
func (p *puller) hostReadRange(client hostReader, key racersdk.Key, dest *memfd, etag *string, meta *racersdk.Metadata) func(context.Context, int64, int64) (io.WriterTo, func() error, error) {
	return func(ctx context.Context, offset, length int64) (io.WriterTo, func() error, error) {
		run := writerToFunc(func(io.Writer) (int64, error) {
			var confirmed int64

			result, err := client.HostRead(ctx, racersdk.Request{Key: key}, dest.file, racersdk.HostReadOptions{
				Offset: offset, Length: length, ETag: *etag,
				DestinationOffset: offset, DestinationLength: dest.size - offset,
				OnPage: func(_, n int64) {
					confirmed += n
					p.metrics.receivedBytes.Add(float64(n))
				},
			})
			if err == nil && meta != nil {
				*meta = result.Metadata
			}

			return confirmed, err
		})

		return run, func() error { return nil }, nil
	}
}

type writerToFunc func(io.Writer) (int64, error)

func (f writerToFunc) WriteTo(w io.Writer) (int64, error) { return f(w) }

// udsHostReadAcquirer reads each blob into a pooled memfd with host-read.
// With range-bytes, it pins the version with Stat and reads parallel ranges;
// otherwise it reads the whole object with one unpinned host-read sized to
// the catalog descriptor.
func (p *puller) udsHostReadAcquirer(client hostReader, pool *memfdPool) func(context.Context, string, ocispec.Descriptor) (blobResponse, error) {
	return func(ctx context.Context, _ string, desc ocispec.Descriptor) (blobResponse, error) {
		key, err := racersdk.ParseKey(desc.Digest.Encoded())
		if err != nil {
			return blobResponse{}, err
		}

		if desc.Size < 1 {
			return blobResponse{}, errors.New("host-read needs a nonempty object")
		}

		body := &hostReadBody{pool: pool}
		size, etag := desc.Size, ""
		rangeBytes, parallel, retries, rotate := desc.Size, 1, 0, int64(0)

		var meta *racersdk.Metadata

		if p.opts.RangeBytes > 0 {
			metadata, err := client.Stat(ctx, racersdk.Request{Key: key})
			if err != nil {
				return blobResponse{}, err
			}

			size, etag = metadata.Size, metadata.ETag
			rangeBytes, parallel, retries, rotate = p.opts.RangeBytes, p.opts.RangeParallel, p.opts.RangeRetries, p.rangeRotation(size)
		} else {
			// An unpinned whole-object read learns its metadata from the response.
			meta = &racersdk.Metadata{}
		}

		if size < 0 {
			return blobResponse{}, fmt.Errorf("invalid object size %d", size)
		}

		body.size = size

		dest, err := pool.get(max(size, 1))
		if err != nil {
			return blobResponse{}, err
		}

		body.dest = dest
		response := blobResponse{body: body, success: true}

		if meta != nil {
			// Filled in by the transfer, before the caller checks them.
			response.totalSize = &meta.Size
			response.etag = &meta.ETag
		} else {
			response.totalSize = &size
			response.etag = &etag
		}

		body.ranged = &rangedBody{
			ctx:        ctx,
			get:        p.hostReadRange(client, key, dest, &etag, meta),
			size:       size,
			rangeBytes: max(rangeBytes, 1),
			parallel:   parallel,
			retries:    retries,
			retryDelay: p.opts.RetryDelay,
			rotate:     rotate,
			onResume:   func() { p.metrics.rangeResumes.Inc() },
		}

		return response, nil
	}
}

// validateDelivery checks the delivery mode against the other pull options.
func validateDelivery(opts pullOptions) error {
	switch opts.Delivery {
	case "", deliveryStream:
		return nil
	case deliveryHostRead:
	default:
		return errors.New("delivery must be stream or host-read")
	}

	if opts.Backend != "uds" {
		return errors.New("delivery=host-read requires backend=uds")
	}

	if opts.DiagnoseIntegrity {
		return errors.New("delivery=host-read does not support diagnose-integrity")
	}

	return nil
}
