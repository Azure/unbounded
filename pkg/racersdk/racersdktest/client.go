// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package racersdktest runs an in-process fake Racer so tests can exercise a
// real [racersdk.Client] and [racersdk.Origin] without a deployed Racer.
package racersdktest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Azure/unbounded/pkg/racersdk"
	"github.com/Azure/unbounded/pkg/racersdk/internal/fakeracer"
	"github.com/Azure/unbounded/pkg/racersdk/internal/sdkhook"
	"github.com/Azure/unbounded/pkg/racersdk/internal/wire"
)

// Limits of the SDK origin server that the fake Racer must respect.
const (
	originHeadRequests = 4
	originHeaderWait   = 5 * time.Second
	originRequestWait  = 60 * time.Second
	originIdleTimeout  = 30 * time.Second
)

// NewClient returns a [racersdk.Client] connected to origin through an
// in-process fake Racer, for testing code that reads objects, implements an
// [racersdk.Origin], or both.
//
// The fake does not cache: every Get and Stat calls origin, through the same
// validation and serving code that [racersdk.ServeOrigin] uses. It is not
// evidence of compatibility with a real Racer.
//
// Everything is stopped and removed when the test ends. NewClient calls
// t.Fatal if it cannot start, for example if TMPDIR is so long that a Unix
// socket path inside it would exceed 107 bytes.
func NewClient(t testing.TB, origin racersdk.Origin) *racersdk.Client {
	t.Helper()

	client, cleanup, err := start(origin)
	if err != nil {
		t.Fatalf("racersdktest: %v", err)
	}

	t.Cleanup(cleanup)

	return client
}

func start(origin racersdk.Origin) (*racersdk.Client, func(), error) {
	if origin == nil {
		return nil, nil, errors.New("nil origin")
	}

	newClient, ok := sdkhook.NewClientAt.(func(racersdk.ClientConfig, string) (*racersdk.Client, error))
	if !ok {
		panic("racersdktest: invalid NewClientAt hook")
	}

	serveOrigin, ok := sdkhook.ServeOriginAt.(func(context.Context, racersdk.OriginConfig, racersdk.Origin, string) error)
	if !ok {
		panic("racersdktest: invalid ServeOriginAt hook")
	}

	const volume = "sdk-fake"

	dir, err := socketDir()
	if err != nil {
		return nil, nil, err
	}

	originPath, clientPath := filepath.Join(dir, "o"), filepath.Join(dir, "c")

	client, err := newClient(racersdk.ClientConfig{Volume: volume}, clientPath)
	if err != nil {
		removeDir(dir)
		return nil, nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	transport := originTransport(ctx, originPath)
	metadataTransport := originTransport(ctx, originPath)
	metadataTransport.MaxConnsPerHost = originHeadRequests
	metadataTransport.MaxIdleConns = originHeadRequests
	metadataTransport.MaxIdleConnsPerHost = originHeadRequests
	handler := &trackedHandler{next: fakeracer.NewHandler(originTransports{bulk: transport, metadata: metadataTransport})}
	server := &http.Server{
		ReadHeaderTimeout: originHeaderWait, IdleTimeout: originIdleTimeout,
		MaxHeaderBytes: wire.MaxHeadBytes, ErrorLog: log.New(io.Discard, "", 0),
		BaseContext: func(net.Listener) context.Context { return ctx },
		Handler:     handler,
	}

	var serving sync.WaitGroup

	cleanup := sync.OnceFunc(func() {
		closeBody(client)
		cancel()
		handler.stop()
		closeBody(server)
		serving.Wait()
		handler.active.Wait()
		transport.CloseIdleConnections()
		metadataTransport.CloseIdleConnections()
		removeDir(dir)
	})
	// Every startup failure after this point uses the same shutdown sequence.
	started := false

	defer func() {
		if !started {
			cleanup()
		}
	}()

	originDone := make(chan error, 1)

	serving.Go(func() {
		originDone <- serveOrigin(ctx, racersdk.OriginConfig{Volume: volume}, origin, originPath)
	})

	if err := waitOrigin(originPath, originDone); err != nil {
		return nil, nil, err
	}

	listener, err := net.Listen("unix", clientPath)
	if err != nil {
		return nil, nil, fmt.Errorf("fake listen: %w", err)
	}

	serving.Go(func() {
		defer closeBody(listener)

		if server.Serve(listener) != nil {
			return
		}
	})

	started = true

	return client, cleanup, nil
}

// socketDir resolves TMPDIR before checking the Unix socket path limit.
// A failed setup removes the directory using its original path.
func socketDir() (string, error) {
	dir, err := os.MkdirTemp("", "rs-")
	if err != nil {
		return "", fmt.Errorf("temporary directory: %w", err)
	}

	ready := false

	defer func() {
		if !ready {
			removeDir(dir)
		}
	}()

	absolute, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("absolute temporary directory: %w", err)
	}

	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", fmt.Errorf("resolve temporary directory: %w", err)
	}

	if length := len(filepath.Join(resolved, "o")); length > 107 {
		return "", fmt.Errorf("unix socket path exceeds 107-byte limit (%d bytes); use a shorter TMPDIR", length)
	}

	ready = true

	return resolved, nil
}

// HEAD must reach the origin's reserved capacity even when every GET is blocked.
type originTransports struct {
	bulk, metadata *http.Transport
}

func (t originTransports) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method == http.MethodHead {
		return t.metadata.RoundTrip(r)
	}

	return t.bulk.RoundTrip(r)
}

func originTransport(ctx context.Context, path string) *http.Transport {
	return &http.Transport{
		DisableCompression: true, MaxConnsPerHost: 16, MaxIdleConns: 16, MaxIdleConnsPerHost: 16,
		ResponseHeaderTimeout: originRequestWait, IdleConnTimeout: originIdleTimeout,
		MaxResponseHeaderBytes: int64(wire.MaxHeadBytes),
		DialContext: func(dialCtx context.Context, _, _ string) (net.Conn, error) {
			dialCtx, stop := context.WithCancel(dialCtx)
			defer stop()

			unhook := context.AfterFunc(ctx, stop)
			defer unhook()

			return (&net.Dialer{Timeout: originHeaderWait}).DialContext(dialCtx, "unix", path)
		},
	}
}

// trackedHandler counts hijacked subscriptions, which http.Server.Close does
// not wait for. stop prevents Add from racing with the cleanup's Wait.
type trackedHandler struct {
	next    http.Handler
	mu      sync.Mutex
	stopped bool
	active  sync.WaitGroup
}

func (h *trackedHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	if h.stopped {
		h.mu.Unlock()
		return
	}

	h.active.Add(1)

	h.mu.Unlock()
	defer h.active.Done()

	h.next.ServeHTTP(w, r)
}

func (h *trackedHandler) stop() {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.stopped = true
}

// A successful dial establishes readiness without invoking the user's callback.
// Polling is bounded and also observes synchronous origin startup failures.
func waitOrigin(path string, done <-chan error) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()

	for {
		conn, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
		if err == nil {
			closeBody(conn)
			return nil
		}

		select {
		case err := <-done:
			return fmt.Errorf("origin startup: %w", err)
		case <-ctx.Done():
			return fmt.Errorf("origin startup: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func closeBody(body io.Closer) {
	if err := body.Close(); err != nil {
		return
	}
}

func removeDir(dir string) {
	if err := os.RemoveAll(dir); err != nil {
		log.Printf("racersdktest: remove temporary directory: %v", err)
	}
}
