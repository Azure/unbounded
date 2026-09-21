// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"log/slog"
	"math"
	"math/rand"
	"sort"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	racer "github.com/Azure/unbounded/pkg/racer"
)

// Immutable CDF shared by workers. Unlike math/rand.Zipf this also handles s<=1.
type zipf []float64

func newZipf(count int, exponent float64) zipf {
	cdf := make(zipf, count)

	var sum float64
	for i := range cdf {
		sum += math.Pow(float64(i+1), -exponent)
		cdf[i] = sum
	}

	for i := range cdf {
		cdf[i] /= sum
	}

	return cdf
}

func (z zipf) sample(r *rand.Rand) int {
	u := r.Float64()
	return sort.Search(len(z), func(i int) bool { return z[i] > u })
}

type metrics struct {
	bytes     prometheus.Counter
	downloads *prometheus.CounterVec
	duration  *prometheus.HistogramVec
}

func newMetrics(reg prometheus.Registerer) *metrics {
	m := &metrics{
		bytes:     prometheus.NewCounter(prometheus.CounterOpts{Name: "racer_loadgen_received_bytes_total", Help: "Payload bytes consumed, including partial failed downloads."}),
		downloads: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "racer_loadgen_downloads_total", Help: "Completed full-object attempts."}, []string{"result"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "racer_loadgen_download_duration_seconds", Help: "Full-object attempt latency, including HEAD and all page reads.",
			Buckets: []float64{.001, .005, .01, .05, .1, .5, 1, 5, 10, 30, 60, 120, 300, 600},
		}, []string{"result"}),
	}
	reg.MustRegister(m.bytes, m.downloads, m.duration)

	for _, result := range []string{"success", "error"} {
		m.downloads.WithLabelValues(result)
		m.duration.WithLabelValues(result)
	}

	return m
}

type discardWriter struct{ bytes prometheus.Counter }

func (w discardWriter) WriteAt(p []byte, _ int64) (int, error) {
	w.bytes.Add(float64(len(p)))
	return len(p), nil
}

func download(ctx context.Context, client *racer.Client, target string, timeout time.Duration, m *metrics) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()
	_, err := client.Download(ctx, target, discardWriter{m.bytes})

	result := "success"
	if err != nil {
		result = "error"
	}

	m.downloads.WithLabelValues(result).Inc()
	m.duration.WithLabelValues(result).Observe(time.Since(start).Seconds())

	return err
}

// One SDK client/pool per object worker keeps idle capacity sized for the total
// page concurrency without introducing a custom transport.
func runLoad(ctx context.Context, c config, d *dataset, m *metrics) error {
	clients := make([]*racer.Client, 0, c.concurrency)

	defer func() {
		for _, client := range clients {
			client.CloseIdleConnections()
		}
	}()

	for i := 0; i < c.concurrency; i++ {
		client, err := racer.NewClient(c.endpoint, racer.ClientOptions{Concurrency: c.pageConcurrency})
		if err != nil {
			return err
		}

		clients = append(clients, client)
	}

	z := newZipf(int(d.count), c.exponent)

	var wg sync.WaitGroup
	for i, client := range clients {
		wg.Add(1)

		go func() {
			defer wg.Done()

			r := rand.New(rand.NewSource(int64(mix(uint64(c.seed) + uint64(i)))))
			for ctx.Err() == nil {
				target := d.target(z.sample(r))
				if err := download(ctx, client, target, c.timeout, m); err != nil {
					if ctx.Err() != nil {
						return
					}
					// The fixed backoff bounds diagnostics to one per worker/second.
					slog.Warn("download failed", "worker", i, "target", target, "error", err)

					timer := time.NewTimer(time.Second)
					select {
					case <-ctx.Done():
						timer.Stop()
						return
					case <-timer.C:
					}
				}
			}
		}()
	}

	wg.Wait()

	return nil
}
