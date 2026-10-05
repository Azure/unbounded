// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"syscall"
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

// This is deliberately separate from the fake dataplane tests. The subprocess
// serves production Rust ClientListeners, Coordinator, RangeStream and Fill over
// a real UDS. Only origin content and initial publication are fixture supplied.
func TestRustSubscriptionInterop(t *testing.T) {
	if os.Getenv("RACER_SUBSCRIPTION_INTEROP") != "1" {
		t.Skip("set RACER_SUBSCRIPTION_INTEROP=1 to run the Rust dataplane fixture")
	}

	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}

	directory := socketDir(t)

	ctx, cancel := context.WithTimeout(t.Context(), 240*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "timeout", "--signal=TERM", "--kill-after=10s", "230s", "cargo", "test", "--locked", "--manifest-path", filepath.Join(root, "cmd/racer-dataplane/Cargo.toml"), "--features", "subscription-interop", "--test", "subscription_interop", "go_sdk_subscription_server", "--", "--exact", "--ignored", "--nocapture")

	cmd.Env = append(os.Environ(), "RACER_SUBSCRIPTION_INTEROP_DIR="+directory)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }

	cmd.WaitDelay = 10 * time.Second
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)

	go func() { done <- cmd.Wait() }()

	reaped := false

	defer func() {
		if reaped {
			return
		}

		if err := os.WriteFile(filepath.Join(directory, "stop"), nil, 0o600); err != nil {
			t.Error(err)
		}

		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Rust fixture: %v", err)
			}
		case <-time.After(15 * time.Second):
			cancel()
			<-done
			t.Error("Rust fixture failed to drain")
		}
	}()

	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for {
		if _, err := os.Stat(filepath.Join(directory, "ready")); err == nil {
			break
		}

		select {
		case err := <-done:
			reaped = true

			t.Fatalf("Rust fixture exited before ready: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-ticker.C:
		}
	}

	// Keep sun_path short even when the shared worktree has a long absolute path.
	anchor, err := os.Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(anchor)

	path := fmt.Sprintf("/proc/%d/fd/%d/interop/client/socket", os.Getpid(), anchor.Fd())

	cache, err := ParseCacheName("interop")
	if err != nil {
		t.Fatal(err)
	}

	client, err := newClient(ClientConfig{Cache: cache, MaxConnections: 1, PageWindow: 1, BodyReadTimeout: 5 * time.Second}, path)
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(client)

	for key, size := range []int64{0, 4096, int64(PageSize), 3*int64(PageSize) + 13} {
		t.Run("Stat/"+strconv.FormatInt(size, 10), func(t *testing.T) {
			metadata, err := client.Stat(ctx, Request{Key: Key{byte(key)}})
			if err != nil || metadata.Size != ByteLength(size) {
				t.Fatal("v2 HEAD metadata", metadata, err)
			}
		})
		t.Run("Get/"+strconv.FormatInt(size, 10), func(t *testing.T) {
			value, err := client.Get(ctx, Request{Key: Key{byte(key)}})
			if err != nil {
				t.Fatal(err)
			}
			defer closeBody(value)

			if !value.stream.ordered || value.Metadata().Size != ByteLength(size) {
				t.Fatal("Get did not negotiate ordered immutable subscription")
			}

			sink := &offsetSink{}

			n, err := io.CopyBuffer(sink, value, make([]byte, 32*1024))
			if err != nil || n != size {
				t.Fatalf("ordered bytes=%d want=%d: %v", n, size, err)
			}
		})
	}

	t.Run("Get/read-ahead-large-bounded", func(t *testing.T) {
		value, err := client.Get(ctx, Request{Key: Key{4}}, ReadOptions{PageCredits: 2, ByteCredits: 2 * PageSize})
		if err != nil {
			t.Fatal(err)
		}
		defer closeBody(value)

		if cap(value.ordered.slots) != 2 {
			t.Fatal("live ordered read did not enable two-buffer read-ahead")
		}

		n, err := io.Copy(&offsetSink{}, value)
		if err != nil || n != 32*int64(PageSize)+13 {
			t.Fatal("large ordered read", n, err)
		}

		orderedClean(t, value)
	})

	for _, credits := range []int{1, 2} {
		t.Run("Get/read-ahead-partial/credits="+strconv.Itoa(credits), func(t *testing.T) {
			options := ReadOptions{Offset: ByteOffset(PageSize - 7), Length: PageSize + 20, PageCredits: credits}

			value, err := client.Get(ctx, Request{Key: Key{3}}, options)
			if err != nil {
				t.Fatal(err)
			}
			defer closeBody(value)

			n, err := io.Copy(&offsetSink{offset: int64(options.Offset)}, value)
			if err != nil || n != int64(options.Length) {
				t.Fatal("partial ordered read", n, err)
			}

			orderedClean(t, value)
		})
	}

	t.Run("OpenPages/partial-release-final", func(t *testing.T) {
		options := ReadOptions{Offset: ByteOffset(PageSize - 7), Length: 2*PageSize + 20, PageCredits: 1, ByteCredits: PageSize}

		stream, err := client.OpenPages(ctx, Request{Key: Key{3}}, options)
		if err != nil {
			t.Fatal(err)
		}
		defer closeBody(stream)

		if stream.ordered {
			t.Fatal("OpenPages default must negotiate unordered delivery")
		}
		// Split every release into one-byte writes so Rust must retain partial
		// 12-byte control frames across reads rather than assume one recv.
		stream.conn.Conn = &fragmentedReleaseConn{Conn: stream.conn.Conn}

		var total int64

		seen := make(map[uint64]bool)

		for {
			page, err := stream.Next()
			if err == io.EOF {
				break
			}

			if err != nil {
				t.Fatal(err)
			}

			if seen[page.Number] {
				t.Fatal("duplicate page", page.Number)
			}

			seen[page.Number] = true
			checkInteropPage(t, page)
			total += int64(len(page.Data))

			assertInteropAccounting(t, stream)

			if !stream.complete {
				// Bypass SDK credit waiting: the actual Rust socket must be
				// silent while this sole page credit is held by the caller.
				if err := stream.conn.SetReadDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
					t.Fatal(err)
				}

				_, err := stream.conn.Reader.Peek(1)

				var timeout net.Error
				if !errors.As(err, &timeout) || !timeout.Timeout() {
					t.Fatalf("Rust sent data without released credit: %v", err)
				}

				if err := stream.conn.SetReadDeadline(time.Time{}); err != nil {
					t.Fatal(err)
				}
			}

			if err := page.Release(); err != nil {
				t.Fatal(err)
			}

			if err := page.Release(); err != nil || page.Data != nil {
				t.Fatal("release is not idempotent", err)
			}
		}

		if total != int64(options.Length) || len(seen) != 4 || !stream.complete {
			t.Fatalf("partial range bytes=%d pages=%d complete=%v", total, len(seen), stream.complete)
		}

		assertInteropAccounting(t, stream)
	})

	t.Run("OpenPages/empty", func(t *testing.T) {
		stream, err := client.OpenPages(ctx, Request{})
		if err != nil {
			t.Fatal(err)
		}
		defer closeBody(stream)

		if page, err := stream.Next(); page != nil || err != io.EOF || !stream.complete {
			t.Fatalf("empty terminal frame: %v %v", page, err)
		}

		assertInteropAccounting(t, stream)
	})

	t.Run("OpenPages/byte-credit-and-final-held", func(t *testing.T) {
		stream, err := client.OpenPages(ctx, Request{Key: Key{3}}, ReadOptions{PageCredits: 2, ByteCredits: PageSize, Length: PageSize + 13})
		if err != nil {
			t.Fatal(err)
		}
		defer closeBody(stream)

		first, err := stream.Next()
		if err != nil {
			t.Fatal(err)
		}

		checkInteropPage(t, first)
		assertInteropAccounting(t, stream)
		// There is still a page credit, but no byte credit. Inspect the Rust
		// socket, not just the SDK's local gate, to verify both endpoints.
		if err := stream.conn.SetReadDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
			t.Fatal(err)
		}

		_, err = stream.conn.Reader.Peek(1)

		var timeout net.Error
		if !errors.As(err, &timeout) || !timeout.Timeout() {
			t.Fatalf("Rust bypassed exhausted byte credit: %v", err)
		}

		if err := stream.conn.SetReadDeadline(time.Time{}); err != nil {
			t.Fatal(err)
		}

		if err := first.Release(); err != nil {
			t.Fatal(err)
		}

		last, err := stream.Next()
		if err != nil {
			t.Fatal(err)
		}

		checkInteropPage(t, last)

		if len(last.Data) != 13 || !stream.complete {
			t.Fatal("final short page must include validated Complete without release")
		}

		if page, err := stream.Next(); page != nil || err != io.EOF {
			t.Fatal("held final lease blocked EOF", err)
		}

		checkInteropPage(t, last)

		if err := last.Release(); err != nil {
			t.Fatal("final release after remote close", err)
		}

		assertInteropAccounting(t, stream)
	})

	t.Run("OpenPages/large-bounded", func(t *testing.T) {
		stream, err := client.OpenPages(ctx, Request{Key: Key{4}}, ReadOptions{PageCredits: 2, ByteCredits: 2 * PageSize})
		if err != nil {
			t.Fatal(err)
		}
		defer closeBody(stream)

		var total int64

		seen := make(map[ByteOffset]bool)

		for {
			page, err := stream.Next()
			if err == io.EOF {
				break
			}

			if err != nil {
				t.Fatal(err)
			}

			if seen[page.Offset] {
				t.Fatal("duplicate offset", page.Offset)
			}

			seen[page.Offset] = true
			checkInteropPage(t, page)
			total += int64(len(page.Data))

			assertInteropAccounting(t, stream)

			if err := page.Release(); err != nil {
				t.Fatal(err)
			}
		}

		if total != 32*int64(PageSize)+13 || len(seen) != 33 {
			t.Fatalf("bytes=%d pages=%d", total, len(seen))
		}

		assertInteropAccounting(t, stream)
	})

	for _, closeStream := range []bool{false, true} {
		t.Run("cancel-held-credit/close="+strconv.FormatBool(closeStream), func(t *testing.T) {
			requestCtx, stop := context.WithCancel(ctx)
			defer stop()

			stream, err := client.OpenPages(requestCtx, Request{Key: Key{5}}, ReadOptions{PageCredits: 1, ByteCredits: PageSize})
			if err != nil {
				t.Fatal(err)
			}
			defer closeBody(stream)

			page, err := stream.Next()
			if err != nil {
				t.Fatal(err)
			}

			checkInteropPage(t, page)
			assertInteropAccounting(t, stream)

			blocked := make(chan error, 1)

			go func() { _, err := stream.Next(); blocked <- err }()

			select {
			case err := <-blocked:
				t.Fatalf("Next bypassed held credit: %v", err)
			case <-time.After(50 * time.Millisecond):
			}

			if closeStream {
				closeBody(stream)
			} else {
				stop()
			}

			select {
			case err := <-blocked:
				if err == nil || err == io.EOF || !closeStream && !errors.Is(err, context.Canceled) {
					t.Fatal("wrong cancellation result", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("cancel retained blocked Next")
			}

			_ = page.Release()

			assertInteropAccounting(t, stream)
			// MaxConnections=1 also verifies canceled admission is returned.
			value, err := client.Get(ctx, Request{Key: Key{1}})
			if err != nil {
				t.Fatal("request after cancellation", err)
			}
			defer closeBody(value)

			if n, err := io.Copy(&offsetSink{}, value); err != nil || n != 4096 {
				t.Fatal("read after cancellation", n, err)
			}
		})
	}

	t.Run("OpenPages/writer-failure", func(t *testing.T) {
		sentinel := errors.New("destination failure")

		stream, err := client.OpenPages(ctx, Request{Key: Key{5}})
		if err != nil {
			t.Fatal(err)
		}
		defer closeBody(stream)

		page, err := stream.Next()
		if err != nil {
			t.Fatal(err)
		}

		n, err := (writeFunc(func([]byte) (int, error) { return 0, sentinel })).Write(page.Data)
		if n != 0 || !errors.Is(err, sentinel) {
			t.Fatal(n, err)
		}

		closeBody(stream)

		_ = page.Release()
		if page.Data != nil || client.Stats().ActiveBulk != 0 {
			t.Fatal("failed destination retained ownership")
		}

		assertInteropAccounting(t, stream)
	})
}

func checkInteropPage(t *testing.T, page *PageLease) {
	t.Helper()

	if page.Number != uint64(page.Offset)/uint64(PageSize) || len(page.Data) > int(PageSize) {
		t.Fatal("invalid page geometry")
	}

	if _, err := (&offsetSink{offset: int64(page.Offset)}).Write(page.Data); err != nil {
		t.Fatal(err)
	}
}

func assertInteropAccounting(t *testing.T, stream *PageStream) {
	t.Helper()
	stream.mu.Lock()
	defer stream.mu.Unlock()

	var held uint64
	for _, length := range stream.outstanding {
		held += uint64(length)
	}

	if held != stream.bytesHeld || held > stream.byteCredits || len(stream.outstanding) > stream.pageCredits || stream.sequence.Intervals() > 4096 {
		t.Fatalf("unbounded SDK accounting: bytes=%d pages=%d intervals=%d", held, len(stream.outstanding), stream.sequence.Intervals())
	}
}

type fragmentedReleaseConn struct{ net.Conn }

func (c *fragmentedReleaseConn) Write(p []byte) (int, error) {
	for i := range p {
		if _, err := c.Conn.Write(p[i : i+1]); err != nil {
			return i, err
		}

		time.Sleep(time.Millisecond)
	}

	return len(p), nil
}
