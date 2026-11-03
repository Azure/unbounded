// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// fake-azure-blob serves deterministic synthetic block blobs at
// http://ADDR/<account>/<container>/<key> for local Racer origin benchmarks.
// It supports HEAD (Get Blob Properties) and GET (Get Blob) with Range or
// x-ms-range and If-Match. The first line on stdout is the listen address.
// GET /_fake/stats returns request and byte counters as JSON. It is not a
// general Azure emulator: there are no writes, listing, or authentication.
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Azure/unbounded/internal/racerbench/fakeblob"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:0", "listen address")
	size := flag.Int64("size", 64<<20, "object size in bytes (keys starting with size-<n>/ override it)")

	flag.Parse()

	if err := run(*listen, *size); err != nil {
		fmt.Fprintln(os.Stderr, "fake-azure-blob:", err)
		os.Exit(1)
	}
}

func run(listen string, size int64) error {
	if size < 0 {
		return fmt.Errorf("negative size %d", size)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	listener, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}

	fake := &fakeblob.Server{Size: size, Modified: time.Now()}
	server := &http.Server{Handler: fake, ReadHeaderTimeout: 10 * time.Second}

	fmt.Println(listener.Addr().String())

	done := make(chan error, 1)

	go func() { done <- server.Serve(listener) }()

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
	}

	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := server.Shutdown(shutdown); err != nil {
		fmt.Fprintln(os.Stderr, "fake-azure-blob: shutdown:", err)
	}

	fmt.Fprintf(os.Stderr, "heads=%d gets=%d errors=%d bytes=%d\n",
		fake.Stats.Heads.Load(), fake.Stats.Gets.Load(), fake.Stats.Errors.Load(), fake.Stats.Bytes.Load())

	return nil
}
