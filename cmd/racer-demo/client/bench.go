// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Azure/unbounded/pkg/racersdk"
)

const (
	documents      = 10000
	concurrency    = 256
	zipfS          = 1.1
	zipfV          = 1.0
	reportInterval = 5 * time.Second
	errorBackoff   = 100 * time.Millisecond
)

func runBench() error {
	ctx := context.TODO()

	client, err := racersdk.NewClient(racersdk.ClientConfig{
		Cache:          cacheName,
		MaxConnections: concurrency,
	})
	if err != nil {
		return err
	}
	defer client.Close()

	// Separate files avoid sharing a destination lock between workers.
	var sinks [concurrency]*os.File
	for i := range sinks {
		sinks[i], err = os.OpenFile(os.DevNull, os.O_WRONLY, 0)
		if err != nil {
			return err
		}

		defer client.Close()
	}

	var (
		bytes   atomic.Uint64
		workers sync.WaitGroup
	)

	firstError := make(chan error, 1)
	start := time.Now()

	for i, sink := range sinks {
		workers.Add(1)

		go func() {
			defer workers.Done()

			rng := rand.New(rand.NewPCG(uint64(i)+1, uint64(i)+1001))
			zipf := rand.NewZipf(rng, zipfS, zipfV, documents-1)

			for ctx.Err() == nil {
				obj, err := client.Get(ctx, documentRequest(zipf.Uint64()))
				if err == nil {
					// Plain io.Copy keeps the SDK splice path. Count partial copies too.
					n, copyErr := io.Copy(sink, obj)
					bytes.Add(uint64(n))

					err = obj.Close()
					if copyErr != nil {
						err = copyErr
					}
				}

				if ctx.Err() != nil {
					return
				}

				if err == nil {
					continue
				}

				select {
				case firstError <- err:
				default:
				}

				timer := time.NewTimer(errorBackoff)
				select {
				case <-ctx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
			}
		}()
	}

	reportError := func() {
		select {
		case err := <-firstError:
			log.Print(err)
		default:
		}
	}

	ticker := time.NewTicker(reportInterval)
	defer ticker.Stop()

	lastTime := start

	var lastBytes uint64

	for {
		select {
		case <-ctx.Done():
			workers.Wait()
			reportError()
			fmt.Printf("average %.3f Gbit/s\n", float64(bytes.Load())*8/1e9/time.Since(start).Seconds())

			return nil
		case <-ticker.C:
			now := time.Now()
			total := bytes.Load()
			fmt.Printf("%.3f Gbit/s\n", float64(total-lastBytes)*8/1e9/now.Sub(lastTime).Seconds())
			reportError()

			lastTime, lastBytes = now, total
		}
	}
}

func documentRequest(n uint64) racersdk.Request {
	id := strconv.FormatUint(n, 10)

	return racersdk.Request{
		Key:      sha256.Sum256([]byte("racer-demo/doc/" + id)),
		Metadata: id,
	}
}
