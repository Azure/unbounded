// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"sync"
	"testing"
	"time"
)

// TestRealRuntimeConnectionAgeLoad is driven by the opt-in Rust process fixture.
// Only socket paths differ from deployment; clocks, jitter, dialing, and reuse
// are production SDK behavior. No fake HTTP server or connection is substituted.
func TestRealRuntimeConnectionAgeLoad(t *testing.T) {
	path := os.Getenv("RACER_SDK_AGE_SOCKET")
	if path == "" {
		t.Skip("requires the real_sdk_connection_age_sustained Rust executable fixture")
	}

	age, err := time.ParseDuration(os.Getenv("RACER_SDK_AGE"))
	if err != nil || age <= 0 {
		t.Fatal("invalid RACER_SDK_AGE", err)
	}

	duration := 8 * time.Second
	if text := os.Getenv("RACER_SDK_AGE_DURATION"); text != "" {
		duration, err = time.ParseDuration(text)
		if err != nil || duration < 4*time.Second || duration > 30*time.Second {
			t.Fatal("duration must be between 4s and 30s", err)
		}
	}

	newLoadClient := func(path string) *Client {
		c, err := newClient(ClientConfig{
			Cache: CacheName{value: "age"}, MaxConnAge: age,
			MaxConnections: 2, MetadataConnections: 1, SmallObjectConnections: 1,
			QueueTimeout: 3 * time.Second,
		}, path)
		if err != nil {
			t.Fatal(err)
		}

		t.Cleanup(func() { closeBody(c) })

		return c
	}
	bulk, small := newLoadClient(path), newLoadClient(os.Getenv("RACER_SDK_AGE_SMALL_SOCKET"))
	request := Request{Context: FetchContext{authorization: Authorization{value: "fixture-credential"}, metadata: AdapterMetadata{value: "fixture-metadata"}}}

	ctx, cancel := context.WithTimeout(context.Background(), duration+10*time.Second)
	defer cancel()

	type measurement struct {
		Kind    string
		Latency time.Duration
		Bytes   int64
		Err     error
	}

	results := make(chan measurement, 32)
	start := time.Now()
	until := start.Add(duration)

	var wg sync.WaitGroup
	// Four bulk producers compete for two slots. One deliberately holds a live
	// response past the rotating age. Fast metadata/small traffic uses separate
	// reservations; a paced bulk producer adds skew rather than lockstep reuse.
	for worker := range 8 {
		wg.Go(func() {
			kind := "bulk"
			if worker >= 6 {
				kind = "small"
			} else if worker >= 4 {
				kind = "stat"
			}

			for time.Now().Before(until) {
				began := time.Now()

				n, err := ageLoadRequest(ctx, bulk, small, request, worker)
				results <- measurement{kind, time.Since(began), n, err}

				if err != nil {
					return
				}

				pause := 10 * time.Millisecond
				if worker == 3 {
					pause = 100 * time.Millisecond
				}

				time.Sleep(pause)
			}
		})
	}

	go func() { wg.Wait(); close(results) }()

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	latencies := map[string][]time.Duration{}

	var (
		bytes                                            int64
		failures, peakQueue, peakActive, peakConnections int
		samples                                          []map[string]any
	)

	for results != nil {
		select {
		case r, ok := <-results:
			if !ok {
				results = nil
				continue
			}

			if r.Err != nil {
				failures++

				t.Errorf("%s: %v", r.Kind, r.Err)
			} else {
				latencies[r.Kind] = append(latencies[r.Kind], r.Latency)
				bytes += r.Bytes
			}
		case <-ticker.C:
			b, s := bulk.Stats(), small.Stats()
			peakQueue = max(peakQueue, b.QueueDepth+s.QueueDepth)
			peakActive = max(peakActive, b.ActiveBulk+b.ActiveMetadata+s.ActiveSmallObjects)

			peakConnections = max(peakConnections, int(b.Connections+s.Connections))
			if len(samples) == 0 || time.Since(start).Seconds() >= float64(len(samples)) {
				samples = append(samples, map[string]any{"seconds": time.Since(start).Seconds(), "bulk": b, "small": s})
			}
		}
	}

	elapsed := time.Since(start).Seconds()
	summary := map[string]any{}
	total := 0

	for kind, values := range latencies {
		sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
		total += len(values)
		summary[kind] = map[string]any{"completed": len(values), "p50_ms": float64(values[(len(values)-1)/2]) / 1e6, "p99_ms": float64(values[(len(values)-1)*99/100]) / 1e6, "max_ms": float64(values[len(values)-1]) / 1e6}
	}

	b, s := bulk.Stats(), small.Stats()

	report, err := json.Marshal(map[string]any{"age": age.String(), "elapsed_seconds": elapsed, "verified_bytes": bytes, "MiB_per_second": float64(bytes) / (1 << 20) / elapsed, "requests_per_second": float64(total) / elapsed, "failures": failures, "latency": summary, "peak_sampled_queue": peakQueue, "peak_sampled_active": peakActive, "peak_sampled_connections": peakConnections, "bulk_stats": b, "small_stats": s, "samples": samples})
	if err != nil {
		t.Fatal(err)
	}

	t.Log("SDK_AGE", string(report))

	if len(latencies) != 3 || peakQueue == 0 || b.QueueWaits == 0 {
		t.Error("mixed traffic or queue pressure not exercised")
	}

	for _, stats := range []Stats{b, s} {
		if stats.Retries != 0 || stats.QueueRejections != 0 || stats.QueueTimeouts != 0 || stats.QueueDepth != 0 || stats.ActiveBulk != 0 || stats.ActiveMetadata != 0 || stats.ActiveSmallObjects != 0 {
			t.Error("unexpected retries, rejection, timeout, or retained admission", stats)
		}

		if age > duration && stats.ConnectionRotations != 0 {
			t.Error("baseline rotated", stats)
		}
	}

	// HEAD connections remain reusable and must still rotate under sustained
	// traffic. POST subscriptions close at completion, including SmallObject;
	// every completed small read must dial once, never reuse or age-rotate.
	if age <= time.Second && b.ConnectionRotations < 3 {
		t.Error("too few metadata rotation cycles", b)
	}

	if b.ConnectionReuses == 0 || b.Dials < uint64(len(latencies["bulk"]))+1 {
		t.Error("metadata reuse or dedicated bulk subscriptions not exercised", b)
	}

	if s.Dials != uint64(len(latencies["small"])) || s.ConnectionReuses != 0 || s.ConnectionRotations != 0 || s.Connections != 0 || s.IdleConnections != 0 {
		t.Error("small subscriptions must use one dedicated connection per read", s)
	}
}

func ageLoadRequest(ctx context.Context, bulk, small *Client, request Request, worker int) (int64, error) {
	if worker == 4 || worker == 5 {
		m, err := bulk.Stat(ctx, request)
		if err == nil && (m.Size != PageSize+113 || m.ETag.String() != `"restart-v1"`) {
			err = fmt.Errorf("incorrect Stat metadata: %+v", m)
		}

		return 0, err
	}

	c, length := bulk, int64(PageSize)+113

	options := ReadOptions{}
	if worker >= 6 {
		c, length, options.SmallObject = small, 113, true
	}
	// Mix whole-object subscriptions with an explicit cross-page range.
	first := int64(0)
	if worker == 3 {
		first, length = int64(PageSize)-41, 97
		options.Offset, options.Length = ByteOffset(first), ByteLength(length)
	}

	v, err := c.Get(ctx, request, options)
	if err != nil {
		return 0, err
	}
	defer closeBody(v)

	wantSize := PageSize + 113
	if worker >= 6 {
		wantSize = 113
	}

	if v.Metadata().Size != wantSize || v.Metadata().ETag.String() != `"restart-v1"` {
		return 0, fmt.Errorf("incorrect Get metadata")
	}

	if worker == 0 {
		// Always exceed the rotating phase's 500 ms maximum, without changing the
		// baseline's workload. A frame remains active, not an idle pooled socket.
		select {
		case <-time.After(1100 * time.Millisecond):
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}

	sink := &agePayloadSink{offset: first}

	n, err := io.Copy(sink, v)
	if err == nil && n != length {
		err = fmt.Errorf("length %d, want %d", n, length)
	}

	return n, err
}

type agePayloadSink struct{ offset int64 }

func TestAgePayloadSinkRejectsCorruption(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		sink := &agePayloadSink{offset: int64(PageSize) - 1}

		data := make([]byte, 3)
		for i := range data {
			offset := sink.offset + int64(i)
			data[i] = byte((offset*31 + offset/int64(PageSize)*17) % 251)
		}

		if corrupt {
			data[1] ^= 1
		}

		n, err := sink.Write(data)
		if corrupt {
			if n != 1 || err == nil {
				t.Fatal("corruption accepted", n, err)
			}
		} else if n != len(data) || err != nil || sink.offset != int64(PageSize)+2 {
			t.Fatal("valid cross-page payload rejected", n, err, sink.offset)
		}
	}
}

func (s *agePayloadSink) Write(p []byte) (int, error) {
	for i, b := range p {
		offset := s.offset + int64(i)

		want := byte((offset*31 + offset/int64(PageSize)*17) % 251)
		if b != want {
			return i, fmt.Errorf("corrupt byte at %d: %d != %d", offset, b, want)
		}
	}

	s.offset += int64(len(p))

	return len(p), nil
}
