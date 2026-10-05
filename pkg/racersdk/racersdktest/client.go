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
// Private temporary Unix sockets require no /run provisioning and add no
// production endpoint overrides. Unlike the original loopback TCP helper, these
// exercise the production socket path while retaining local, isolated startup.
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

	cache, err := racersdk.ParseCacheName("sdk-fake")
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

	config, err := defaults(racersdk.OriginConfig{Cache: cache})
	if err != nil {
		return nil, nil, err
	}

	dir, err := os.MkdirTemp("", "rs-")
	if err != nil {
		return nil, nil, fmt.Errorf("racersdktest: temporary directory: %w", err)
	}

	absolute, err := filepath.Abs(dir)
	if err != nil {
		removeDir(dir)
		return nil, nil, fmt.Errorf("racersdktest: absolute temporary directory: %w", err)
	}

	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		removeDir(dir)
		return nil, nil, fmt.Errorf("racersdktest: resolve temporary directory: %w", err)
	}

	dir = resolved

	originPath, clientPath := filepath.Join(dir, "o"), filepath.Join(dir, "c")
	if len(originPath) > 107 || len(clientPath) > 107 {
		removeDir(dir)
		return nil, nil, fmt.Errorf("racersdktest: Unix socket path exceeds 107-byte limit (%d bytes); use a shorter TMPDIR", max(len(originPath), len(clientPath)))
	}

	client, err := newClient(racersdk.ClientConfig{Cache: cache}, clientPath)
	if err != nil {
		removeDir(dir)
		return nil, nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	transport := &http.Transport{
		DisableCompression: true, MaxConnsPerHost: 16, MaxIdleConns: 16, MaxIdleConnsPerHost: 16,
		ResponseHeaderTimeout: config.RequestTimeout, IdleConnTimeout: config.IdleTimeout,
		MaxResponseHeaderBytes: int64(wire.MaxHeadBytes),
		DialContext: func(dialCtx context.Context, _, _ string) (net.Conn, error) {
			dialCtx, stop := context.WithCancel(dialCtx)
			defer stop()

			unhook := context.AfterFunc(ctx, stop)
			defer unhook()

			return (&net.Dialer{Timeout: config.ReadHeaderTimeout}).DialContext(dialCtx, "unix", originPath)
		},
	}

	var (
		serving, handlers sync.WaitGroup
		once              sync.Once
		mu                sync.Mutex
	)

	stopped := false
	handler := fakeracer.NewHandler(transport)
	server := &http.Server{
		ReadHeaderTimeout: config.ReadHeaderTimeout, IdleTimeout: config.IdleTimeout,
		MaxHeaderBytes: wire.MaxHeadBytes, ErrorLog: log.New(io.Discard, "", 0),
		BaseContext: func(net.Listener) context.Context { return ctx },
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			if stopped {
				mu.Unlock()
				return
			}

			handlers.Add(1)
			mu.Unlock()

			defer handlers.Done()

			handler.ServeHTTP(w, r)
		}),
	}
	cleanup := func() {
		once.Do(func() {
			closeBody(client)

			cancel()
			mu.Lock()
			stopped = true
			mu.Unlock()

			closeBody(server)

			serving.Wait()
			handlers.Wait()
			transport.CloseIdleConnections()

			removeDir(dir)
		})
	}
	originDone := make(chan error, 1)

	serving.Go(func() {
		originDone <- serveOrigin(ctx, config, origin, originPath)
	})

	if err := waitOrigin(originPath, originDone); err != nil {
		cleanup()
		return nil, nil, err
	}

	listener, err := net.Listen("unix", clientPath)
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("racersdktest: fake listen: %w", err)
	}

	serving.Go(func() {
		defer closeBody(listener)

		if err := server.Serve(listener); err != nil {
			return
		}
	})

	return client, cleanup, nil
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
