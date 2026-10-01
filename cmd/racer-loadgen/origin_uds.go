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
	"strconv"
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

// startUDSOrigin returns only after this invocation's callback has served a HEAD
// and the matching HTTP response has passed status and metadata validation.
// Merely finding a socket (possibly a competing owner) is not readiness. stop
// cancels and joins ServeOrigin so its owned-socket cleanup finishes before exit.
func startUDSOrigin(ctx, startup context.Context, config racersdk.OriginConfig, origin racersdk.Origin, probe racersdk.Key, failed func(error)) (func(), error) {
	if err := prepareOriginDirectory(config.Cache); err != nil {
		return nil, err
	}

	return startUDSOriginOnPath(ctx, startup, "/run/racer/"+config.Cache.String()+"/origin/socket", origin, probe, failed,
		func(ctx context.Context, origin racersdk.Origin) error {
			return racersdk.ServeOrigin(ctx, config, origin)
		})
}

// The path/serve seam keeps production on SDK-owned canonical sockets while
// allowing lifecycle and HTTP readiness tests on temporary Unix listeners.
func startUDSOriginOnPath(ctx, startup context.Context, path string, origin racersdk.Origin, probe racersdk.Key, failed func(error), serve func(context.Context, racersdk.Origin) error) (func(), error) {
	lifetime, cancel := context.WithCancel(ctx)
	done := make(chan struct{})

	var witness atomic.Pointer[racersdk.Metadata]

	go func() {
		defer close(done)

		err := serve(lifetime, func(ctx context.Context, request racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
			metadata, body, err := origin(ctx, request)
			if err == nil && body == nil && metadata.Validate() == nil && request.Operation() == racersdk.OperationHead && request.Key() == probe {
				witness.Store(&metadata)
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

	stopLifetimeProbe := context.AfterFunc(lifetime, stopProbe)
	defer stopLifetimeProbe()

	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", path)
	}}
	defer transport.CloseIdleConnections()

	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	for {
		req, err := http.NewRequestWithContext(probeCtx, http.MethodHead, "http://racer/v1/objects/"+probe.String(), nil)
		if err != nil {
			stop()
			return nil, err
		}

		response, err := client.Do(req)
		if err == nil {
			validationErr := validateOriginProbe(response, probe, witness.Load())

			closeErr := response.Body.Close()
			if validationErr != nil || closeErr != nil {
				stop()
				return nil, errors.Join(validationErr, closeErr)
			}
		}

		if probeCtx.Err() != nil || lifetime.Err() != nil {
			stop()
			return nil, fmt.Errorf("origin readiness: %w", errors.Join(probeCtx.Err(), lifetime.Err()))
		}

		select {
		case <-done:
			stop()
			return nil, errors.New("origin stopped during startup")
		default:
		}

		if err == nil {
			return stop, nil
		}

		if !waitPullDelay(probeCtx, 10*time.Millisecond) {
			stop()
			return nil, fmt.Errorf("origin readiness: %w", probeCtx.Err())
		}
	}
}

func validateOriginProbe(response *http.Response, probe racersdk.Key, witness *racersdk.Metadata) error {
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("origin readiness: HEAD status %d", response.StatusCode)
	}

	if witness == nil || witness.Validate() != nil {
		return errors.New("origin readiness: missing valid owned callback witness")
	}

	if response.ContentLength != int64(witness.Size) || response.Header.Get("ETag") != `"sha256:`+probe.String()+`"` || response.Header.Get("ETag") != witness.ETag.String() || response.Header.Get("Racer-Content-Type") != witness.ContentType {
		return errors.New("origin readiness: HEAD metadata mismatch")
	}

	expires, err := strconv.ParseInt(response.Header.Get("Racer-Expires-At"), 10, 64)
	if err != nil || expires < 0 || expires != witness.ExpiresAt.UnixMilli() {
		return errors.New("origin readiness: invalid expiry metadata")
	}

	return nil
}
