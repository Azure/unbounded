// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package racersdktest provides a local, noncaching Racer test helper backed by
// the real SDK client and origin server. It does not require a deployed Racer.
package racersdktest

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Azure/unbounded/pkg/racersdk"
	"github.com/Azure/unbounded/pkg/racersdk/internal/fakeracer"
	"github.com/Azure/unbounded/pkg/racersdk/internal/sdkhook"
	"github.com/Azure/unbounded/pkg/racersdk/internal/wire"
)

// NewClient returns a real Client backed by a sequential, noncaching fake Racer
// and the SDK's real origin validation/serving machinery. It is a test helper,
// not a Racer implementation or evidence of real Racer compatibility. Client
// and origin resource defaults apply. A nil origin is invalid.
//
// Private temporary Unix sockets exercise the production socket path without
// /run provisioning or production endpoint overrides.
// TMPDIR must permit socket paths of at most 107 bytes; longer paths fail clearly.
//
// The fake forwards request metadata and authorization unchanged. It supports
// v2 HEAD and credit-controlled subscriptions, forwarding pinned continuations
// as whole-page origin requests without object-sized buffering. Origin callback
// errors before response headers retain their HTTP classification; later errors
// abort the stream. Origin must obey racersdk.ServeOrigin's cancellation/body
// ownership contract.
//
// Always call the returned concurrent-safe, idempotent cleanup function (for
// example with t.Cleanup). It closes the Client, cancels origin work, stops both
// servers, waits for serving and fake subscription goroutines, and removes the
// temporary directory. Client.Close alone does not stop the servers. Cleanup
// does not wait for callbacks that ignore cancellation; late bodies are closed.
func NewClient(origin racersdk.Origin) (*racersdk.Client, func(), error) {
	if origin == nil {
		return nil, nil, sdkhook.InvalidOrigin()
	}

	volume, err := racersdk.ParseVolumeName("sdk-fake")
	if err != nil {
		return nil, nil, err
	}

	defaults, ok := sdkhook.OriginDefaults.(func(racersdk.OriginConfig) (racersdk.OriginConfig, error))
	if !ok {
		panic("racersdktest: invalid OriginDefaults hook")
	}

	newClient, ok := sdkhook.NewClientAt.(func(racersdk.ClientConfig, string) (*racersdk.Client, error))
	if !ok {
		panic("racersdktest: invalid NewClientAt hook")
	}

	serveOrigin, ok := sdkhook.ServeOriginAt.(func(context.Context, racersdk.OriginConfig, racersdk.Origin, string) error)
	if !ok {
		panic("racersdktest: invalid ServeOriginAt hook")
	}

	config, err := defaults(racersdk.OriginConfig{Volume: volume})
	if err != nil {
		return nil, nil, err
	}

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
	transport := originTransport(ctx, config, originPath)
	handler := &trackedHandler{next: fakeracer.NewHandler(transport)}
	server := &http.Server{
		ReadHeaderTimeout: config.ReadHeaderTimeout, IdleTimeout: config.IdleTimeout,
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
		originDone <- serveOrigin(ctx, config, origin, originPath)
	})

	if err := waitOrigin(originPath, originDone); err != nil {
		return nil, nil, err
	}

	listener, err := net.Listen("unix", clientPath)
	if err != nil {
		return nil, nil, fmt.Errorf("racersdktest: fake listen: %w", err)
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
		return "", fmt.Errorf("racersdktest: temporary directory: %w", err)
	}

	ready := false

	defer func() {
		if !ready {
			removeDir(dir)
		}
	}()

	absolute, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("racersdktest: absolute temporary directory: %w", err)
	}

	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", fmt.Errorf("racersdktest: resolve temporary directory: %w", err)
	}

	if length := len(filepath.Join(resolved, "o")); length > 107 {
		return "", fmt.Errorf("racersdktest: Unix socket path exceeds 107-byte limit (%d bytes); use a shorter TMPDIR", length)
	}

	ready = true

	return resolved, nil
}

func originTransport(ctx context.Context, config racersdk.OriginConfig, path string) *http.Transport {
	return &http.Transport{
		DisableCompression: true, MaxConnsPerHost: 16, MaxIdleConns: 16, MaxIdleConnsPerHost: 16,
		ResponseHeaderTimeout: config.RequestTimeout, IdleConnTimeout: config.IdleTimeout,
		MaxResponseHeaderBytes: int64(wire.MaxHeadBytes),
		DialContext: func(dialCtx context.Context, _, _ string) (net.Conn, error) {
			dialCtx, stop := context.WithCancel(dialCtx)
			defer stop()

			unhook := context.AfterFunc(ctx, stop)
			defer unhook()

			return (&net.Dialer{Timeout: config.ReadHeaderTimeout}).DialContext(dialCtx, "unix", path)
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
			return fmt.Errorf("racersdktest: origin startup: %w", err)
		case <-ctx.Done():
			return fmt.Errorf("racersdktest: origin startup: %w", ctx.Err())
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
