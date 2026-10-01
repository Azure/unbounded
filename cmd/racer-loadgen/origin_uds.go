// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Azure/unbounded/pkg/racersdk"
)

func syntheticOrigin(catalog *blobCatalog, metrics *loadMetrics) (racersdk.Origin, racersdk.Key, error) {
	blobs := make(map[racersdk.Key]blobSource, len(catalog.blobs)+len(catalog.images))

	var probe racersdk.Key

	add := func(blob blobSource) error {
		key, err := racersdk.ParseKey(blob.descriptor.Digest.Encoded())
		if err != nil {
			return err
		}

		blobs[key] = blob
		probe = key

		return nil
	}
	for _, blob := range catalog.blobs {
		if err := add(blob); err != nil {
			return nil, probe, err
		}
	}

	for _, img := range catalog.images {
		if err := add(blobSource{descriptor: img.Manifest, data: bytes.NewReader(img.manifest)}); err != nil {
			return nil, probe, err
		}
	}

	origin := func(ctx context.Context, request racersdk.OriginRequest) (metadata racersdk.Metadata, body io.ReadCloser, err error) {
		start := time.Now()

		method, code := "GET", "206"
		if request.Operation() == racersdk.OperationHead {
			method, code = "HEAD", "200"
		}

		defer func() {
			if err != nil {
				code = "error"
			}

			if body == nil {
				metrics.originDuration.Observe(time.Since(start).Seconds())
			}

			metrics.originRequests.WithLabelValues(method, code).Inc()
		}()

		if err := ctx.Err(); err != nil {
			return metadata, nil, err
		}

		blob, ok := blobs[request.Key()]
		if !ok {
			return metadata, nil, racersdk.NewOriginError(racersdk.ErrorNotFound, nil)
		}

		tag, err := racersdk.ParseETag(`"` + blob.descriptor.Digest.String() + `"`)
		if err != nil {
			return metadata, nil, err
		}

		metadata = racersdk.Metadata{
			Size: racersdk.ByteLength(blob.descriptor.Size), ETag: tag,
			ExpiresAt: time.Now().Add(24 * time.Hour).Truncate(time.Millisecond), ContentType: blob.descriptor.MediaType,
		}
		if pin, pinned := request.Pin(); pinned && pin != tag {
			return metadata, nil, racersdk.NewOriginError(racersdk.ErrorVersionUnavailable, nil)
		}

		if request.Operation() == racersdk.OperationHead {
			return metadata, nil, nil
		}

		page, _ := request.Range()

		first, last, err := page.Resolve(metadata.Size)
		if err != nil {
			return metadata, nil, err
		}

		return metadata, &originBlobBody{ctx: ctx, reader: io.NewSectionReader(blob.data, int64(first), int64(last-first)+1), metrics: metrics, start: start}, nil
	}

	return origin, probe, nil
}

// Read only generates one bounded chunk at a time. Close is concurrent-safe and
// prevents subsequent generation; neither cancellation nor Close waits on I/O.
type originBlobBody struct {
	ctx     context.Context
	reader  *io.SectionReader
	closed  atomic.Bool
	once    sync.Once
	metrics *loadMetrics
	start   time.Time
}

func (b *originBlobBody) Read(p []byte) (int, error) {
	if err := b.ctx.Err(); err != nil {
		return 0, err
	}

	if b.closed.Load() {
		return 0, os.ErrClosed
	}

	n, err := b.reader.Read(p[:min(len(p), 128*1024)])
	b.metrics.originBytes.Add(float64(n))

	return n, err
}

func (b *originBlobBody) Close() error {
	b.closed.Store(true)
	b.once.Do(func() { b.metrics.originDuration.Observe(time.Since(b.start).Seconds()) })

	return nil
}

// Provision only missing directories. Refuse symlinks and untrusted writable
// ancestors, and leave ownership/modes of existing directories untouched. The
// SDK additionally checks final directory ownership before acquiring its lock.
func prepareOriginDirectory(cache racersdk.CacheName) error {
	path := "/"
	for _, part := range []string{"run", "racer", cache.String(), "origin"} {
		path = filepath.Join(path, part)
		if err := os.Mkdir(path, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}

		info, err := os.Lstat(path)
		if err != nil {
			return err
		}

		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || !info.IsDir() || info.Mode().Perm()&0o022 != 0 || (stat.Uid != 0 && stat.Uid != uint32(os.Geteuid())) {
			return fmt.Errorf("unsafe origin directory %s", path)
		}
	}

	return nil
}

// startUDSOrigin returns only after this invocation's callback has served a HEAD.
// Merely finding a socket (possibly a competing owner) is not readiness. stop
// cancels and joins ServeOrigin so its owned-socket cleanup finishes before exit.
func startUDSOrigin(ctx, startup context.Context, config racersdk.OriginConfig, origin racersdk.Origin, probe racersdk.Key, failed func(error)) (func(), error) {
	if err := prepareOriginDirectory(config.Cache); err != nil {
		return nil, err
	}

	lifetime, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	ready := make(chan struct{})

	var once sync.Once

	go func() {
		defer close(done)

		err := racersdk.ServeOrigin(lifetime, config, func(ctx context.Context, request racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
			metadata, body, err := origin(ctx, request)
			if err == nil && request.Operation() == racersdk.OperationHead {
				once.Do(func() { close(ready) })
			}

			return metadata, body, err
		})
		if lifetime.Err() == nil {
			if err == nil {
				err = errors.New("origin stopped unexpectedly")
			}

			failed(err)
		}
	}()

	stop := func() { cancel(); <-done }
	// Startup is independently bounded even if catalog hashing has no deadline.
	probeCtx, stopProbe := context.WithTimeout(startup, 10*time.Second)
	defer stopProbe()

	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", "/run/racer/"+config.Cache.String()+"/origin/socket")
	}}
	defer transport.CloseIdleConnections()

	client := &http.Client{Transport: transport}

	for {
		req, err := http.NewRequestWithContext(probeCtx, http.MethodHead, "http://racer/v1/objects/"+probe.String(), nil)
		if err != nil {
			stop()
			return nil, err
		}

		response, err := client.Do(req)
		if err == nil {
			if err := response.Body.Close(); err != nil {
				stop()
				return nil, err
			}
		}

		select {
		case <-done:
			stop()
			return nil, errors.New("origin stopped during startup")
		case <-ready:
			return stop, nil
		default:
		}

		if !waitPullDelay(probeCtx, 10*time.Millisecond) {
			stop()
			return nil, fmt.Errorf("origin readiness: %w", probeCtx.Err())
		}
	}
}
