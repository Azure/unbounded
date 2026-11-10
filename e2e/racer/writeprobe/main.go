//go:build e2e

// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Command writeprobe drives Racer writes and reads through pkg/racersdk from
// inside a kind node. The write e2e test cross-compiles it, copies it into a
// client pod that mounts the cache's client socket, and runs one subcommand
// per step. Object bytes are a deterministic stream derived from a seed, so
// the writer and a reader on another node agree on the content without
// moving it through the test host.
//
// Exit codes: 0 success, 1 usage or unexpected error, 3 ErrExists,
// 4 ErrNotFound, 5 content mismatch.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"strconv"
	"time"

	"github.com/Azure/unbounded/pkg/racersdk"
)

const (
	exitExists   = 3
	exitNotFound = 4
	exitMismatch = 5
)

func main() {
	if len(os.Args) < 4 {
		fail(1, errors.New("usage: writeprobe <put|get|stat|delete|persist> <cache> <key> [args]"))
	}

	op, cache, keyText := os.Args[1], os.Args[2], os.Args[3]

	key, err := racersdk.ParseKey(keyText)
	if err != nil {
		fail(1, err)
	}

	client, err := racersdk.NewClient(racersdk.ClientConfig{Cache: cache})
	if err != nil {
		fail(1, err)
	}

	defer client.Close() //nolint:errcheck // Process exit releases everything.

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	request := racersdk.Request{Key: key, Metadata: os.Getenv("WRITEPROBE_METADATA")}
	args := os.Args[4:]

	switch op {
	case "put":
		// put <cache> <key> <size> <seed> <replicas> <upload>
		size, seed := sizeSeed(args)

		options := racersdk.WriteOptions{RetryBusy: time.Minute}
		if len(args) > 2 {
			options.Replicas = atoi(args[2])
		}

		if len(args) > 3 {
			options.Upload = racersdk.UploadMode(args[3])
		}

		result, err := client.Put(ctx, request, io.LimitReader(newStream(seed), size), size, options)
		check(err)
		fmt.Printf("etag=%s state=%s\n", result.ETag, result.UploadState)
	case "get":
		// get <cache> <key> <size> <seed>
		size, seed := sizeSeed(args)

		object, err := client.Get(ctx, request)
		check(err)

		got := sha256.New()
		n, err := io.Copy(got, object)
		check(err)
		check(object.Close())

		want := sha256.New()
		if _, err := io.Copy(want, io.LimitReader(newStream(seed), size)); err != nil {
			fail(1, err)
		}

		if n != size || hex.EncodeToString(got.Sum(nil)) != hex.EncodeToString(want.Sum(nil)) {
			fail(exitMismatch, fmt.Errorf("read %d bytes sha256 %x, want %d bytes sha256 %x",
				n, got.Sum(nil), size, want.Sum(nil)))
		}

		fmt.Printf("read-ok bytes=%d sha256=%x\n", n, got.Sum(nil))
	case "stat":
		meta, err := client.Stat(ctx, request)
		check(err)
		fmt.Printf("stat %+v\n", meta)
	case "delete":
		check(client.Delete(ctx, request))
		fmt.Println("delete-ok")
	case "persist":
		state, err := client.Persist(ctx, request)
		check(err)
		fmt.Printf("state=%s\n", state)
	default:
		fail(1, fmt.Errorf("unknown op %q", op))
	}
}

// newStream returns an endless deterministic byte stream for seed.
func newStream(seed uint64) io.Reader {
	var key [32]byte
	for i := range 8 {
		key[i] = byte(seed >> (8 * i))
	}

	return rand.NewChaCha8(key)
}

func sizeSeed(args []string) (int64, uint64) {
	if len(args) < 2 {
		fail(1, errors.New("missing <size> <seed>"))
	}

	size, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		fail(1, err)
	}

	seed, err := strconv.ParseUint(args[1], 10, 64)
	if err != nil {
		fail(1, err)
	}

	return size, seed
}

func atoi(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		fail(1, err)
	}

	return n
}

func check(err error) {
	switch {
	case err == nil:
	case errors.Is(err, racersdk.ErrExists):
		fail(exitExists, err)
	case errors.Is(err, racersdk.ErrNotFound):
		fail(exitNotFound, err)
	default:
		fail(1, err)
	}
}

func fail(code int, err error) {
	fmt.Fprintln(os.Stderr, "writeprobe:", err)
	os.Exit(code)
}
