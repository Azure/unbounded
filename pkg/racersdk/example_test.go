// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/Azure/unbounded/pkg/racersdk"
)

// These examples need a Racer cache, so they compile but do not run. Tests
// can use racersdktest instead.

var key, _ = racersdk.ParseKey("2c26b46b68ffc68ff99b453c1d30413413422d706483bfa0f98a5e886266e7ae")

// Read copies the object into your buffer, for when you need to look at the
// bytes, here to hash them.
func ExampleObject_Read() { //nolint:testableexamples // Requires a running Racer cache.
	client, err := racersdk.NewClient(racersdk.ClientConfig{Cache: "cache"})
	if err != nil {
		log.Fatal(err)
	}
	defer client.Close()

	object, err := client.Get(context.Background(), racersdk.Request{Key: key})
	if err != nil {
		log.Fatal(err)
	}
	defer object.Close()

	hash := sha256.New()
	if _, err := io.CopyBuffer(hash, object, make([]byte, 256<<10)); err != nil {
		log.Fatal(err)
	}

	fmt.Printf("%x\n", hash.Sum(nil))
}

// WriteTo forwards the object without copying it through your process,
// here to an HTTP response.
func ExampleObject_WriteTo() { //nolint:testableexamples // Requires a running Racer cache.
	client, err := racersdk.NewClient(racersdk.ClientConfig{Cache: "cache"})
	if err != nil {
		log.Fatal(err)
	}
	defer client.Close()

	http.HandleFunc("/blob", func(w http.ResponseWriter, r *http.Request) {
		object, err := client.Get(r.Context(), racersdk.Request{Key: key})
		if errors.Is(err, racersdk.ErrNotFound) {
			http.NotFound(w, r)
			return
		} else if err != nil {
			http.Error(w, "unavailable", http.StatusBadGateway)
			return
		}
		defer object.Close()

		w.Header().Set("Content-Length", strconv.FormatInt(object.Metadata().Size, 10))

		if _, err := object.WriteTo(w); err != nil {
			// Headers are sent; abort so the client sees a truncated body.
			panic(http.ErrAbortHandler)
		}
	})
}

// A later range of a version found earlier, pinned by its ETag.
func ExampleClient_Stat() { //nolint:testableexamples // Requires a running Racer cache.
	client, err := racersdk.NewClient(racersdk.ClientConfig{Cache: "cache"})
	if err != nil {
		log.Fatal(err)
	}
	defer client.Close()

	ctx := context.Background()

	metadata, err := client.Stat(ctx, racersdk.Request{Key: key})
	if err != nil {
		log.Fatal(err)
	}

	object, err := client.Get(ctx, racersdk.Request{Key: key}, racersdk.ReadOptions{
		Offset: metadata.Size / 2,
		ETag:   metadata.ETag,
	})
	if errors.Is(err, racersdk.ErrVersionMismatch) {
		log.Fatal("object changed since Stat")
	} else if err != nil {
		log.Fatal(err)
	}
	defer object.Close()

	if _, err := io.Copy(os.Stdout, object); err != nil {
		log.Fatal(err)
	}
}

// An origin serving a single in-memory object.
func ExampleServeOrigin() { //nolint:testableexamples // Requires a provisioned Racer origin directory.
	origin := exampleMemoryOrigin([]byte("hello, racer"))
	if err := racersdk.ServeOrigin(context.Background(), racersdk.OriginConfig{Cache: "cache"}, origin); err != nil {
		log.Fatal(err)
	}
}

func exampleMemoryOrigin(content []byte) racersdk.Origin {
	metadata := racersdk.Metadata{
		Size:      int64(len(content)),
		ETag:      `"v1"`,
		ExpiresAt: time.Now().Add(time.Hour),
	}

	return func(_ context.Context, r racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
		if r.Key != key {
			return racersdk.Metadata{}, nil, racersdk.ErrNotFound
		}

		if r.ETag != "" && r.ETag != metadata.ETag {
			return racersdk.Metadata{}, nil, racersdk.ErrVersionMismatch
		}

		if r.Head {
			return metadata, nil, nil
		}

		start := min(r.Offset, metadata.Size)

		length := min(r.Length, metadata.Size-start)
		if length == 0 {
			return metadata, nil, nil
		}

		return metadata, io.NopCloser(bytes.NewReader(content[start : start+length])), nil
	}
}
