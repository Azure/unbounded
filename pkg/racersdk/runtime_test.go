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

// The tests in this file are driven by opt-in Rust dataplane fixtures in
// cmd/racer-dataplane/tests. They skip unless those fixtures run them.

// TestRealRuntimeConnectionAgeLoad is driven by the real_sdk_connection_age_sustained
// Rust executable fixture. Only socket paths and the connection age differ from
// deployment; no fake HTTP server or connection is substituted.
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
		c, err := newClient(ClientConfig{Cache: "age", MaxConnections: 2}, path)
		if err != nil {
			t.Fatal(err)
		}

		c.limits.maxConnAge = age
		c.limits.queueTimeout = 3 * time.Second
		c.stat = newLane(1, statQueue)
		c.small = newLane(1, smallQueue)

		t.Cleanup(func() { closeQuietly(c) })

		return c
	}
	bulk, small := newLoadClient(path), newLoadClient(os.Getenv("RACER_SDK_AGE_SMALL_SOCKET"))
	request := Request{Metadata: "fixture-metadata", Authorization: "fixture-credential"}

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

	latencies := map[string][]time.Duration{}

	var (
		bytes    int64
		failures int
	)

	for r := range results {
		if r.Err != nil {
			failures++

			t.Errorf("%s: %v", r.Kind, r.Err)

			continue
		}

		latencies[r.Kind] = append(latencies[r.Kind], r.Latency)
		bytes += r.Bytes
	}

	elapsed := time.Since(start).Seconds()
	summary := map[string]any{}
	total := 0

	for kind, values := range latencies {
		sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
		total += len(values)
		summary[kind] = map[string]any{"completed": len(values), "p50_ms": float64(values[(len(values)-1)/2]) / 1e6, "p99_ms": float64(values[(len(values)-1)*99/100]) / 1e6, "max_ms": float64(values[len(values)-1]) / 1e6}
	}

	report, err := json.Marshal(map[string]any{"age": age.String(), "elapsed_seconds": elapsed, "verified_bytes": bytes, "MiB_per_second": float64(bytes) / (1 << 20) / elapsed, "requests_per_second": float64(total) / elapsed, "failures": failures, "latency": summary})
	if err != nil {
		t.Fatal(err)
	}

	t.Log("SDK_AGE", string(report))

	if len(latencies) != 3 {
		t.Error("mixed traffic not exercised")
	}

	// Admission must be fully returned: every lane accepts a request again.
	for _, c := range []*Client{bulk, small} {
		for _, l := range []*lane{&c.bulk, &c.small, &c.stat} {
			if len(l.slots) != 0 || len(l.queue) != 0 {
				t.Error("retained admission")
			}
		}
	}
}

func ageLoadRequest(ctx context.Context, bulk, small *Client, request Request, worker int) (int64, error) {
	if worker == 4 || worker == 5 {
		m, err := bulk.Stat(ctx, request)
		if err == nil && (m.Size != PageSize+113 || m.ETag != `"restart-v1"`) {
			err = fmt.Errorf("incorrect Stat metadata: %+v", m)
		}

		return 0, err
	}

	c, length := bulk, int64(PageSize)+113

	options := ReadOptions{}
	if worker >= 6 {
		c, length, options.SmallObject = small, 113, true
	}
	// Mix whole-object reads with an explicit cross-page range.
	first := int64(0)
	if worker == 3 {
		first, length = int64(PageSize)-41, 97
		options.Offset, options.Length = first, length
	}

	v, err := c.Get(ctx, request, options)
	if err != nil {
		return 0, err
	}
	defer closeQuietly(v)

	wantSize := int64(PageSize + 113)
	if worker >= 6 {
		wantSize = 113
	}

	if v.Metadata().Size != wantSize || v.Metadata().ETag != `"restart-v1"` {
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

	var n int64
	if worker%2 == 0 {
		n, err = io.Copy(sink, v)
	} else {
		n, err = io.CopyBuffer(sink, readerOnly{v}, make([]byte, 64<<10))
	}

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
	defer closeQuietly(anchor)

	path := fmt.Sprintf("/proc/%d/fd/%d/interop/client/socket", os.Getpid(), anchor.Fd())

	client, err := newClient(ClientConfig{Cache: "interop", MaxConnections: 1}, path)
	if err != nil {
		t.Fatal(err)
	}
	defer closeQuietly(client)

	client.limits.bodyTimeout = 5 * time.Second

	for key, size := range []int64{0, 4096, int64(PageSize), 3*int64(PageSize) + 13} {
		t.Run("Stat/"+strconv.FormatInt(size, 10), func(t *testing.T) {
			metadata, err := client.Stat(ctx, Request{Key: Key{byte(key)}})
			if err != nil || metadata.Size != size {
				t.Fatal("v2 HEAD metadata", metadata, err)
			}
		})

		for _, mode := range []string{"Read", "WriteTo"} {
			t.Run("Get/"+mode+"/"+strconv.FormatInt(size, 10), func(t *testing.T) {
				object, err := client.Get(ctx, Request{Key: Key{byte(key)}})
				if err != nil {
					t.Fatal(err)
				}
				defer closeQuietly(object)

				if object.Metadata().Size != size {
					t.Fatal("wrong metadata", object.Metadata())
				}

				n, err := interopCopy(mode, &offsetSink{}, object)
				if err != nil || n != size {
					t.Fatalf("bytes=%d want=%d: %v", n, size, err)
				}
			})
		}
	}

	t.Run("Get/large", func(t *testing.T) {
		object, err := client.Get(ctx, Request{Key: Key{4}})
		if err != nil {
			t.Fatal(err)
		}
		defer closeQuietly(object)

		n, err := io.Copy(&offsetSink{}, object)
		if err != nil || n != 32*int64(PageSize)+13 {
			t.Fatal("large read", n, err)
		}
	})

	for _, mode := range []string{"Read", "WriteTo"} {
		t.Run("Get/partial/"+mode, func(t *testing.T) {
			options := ReadOptions{Offset: PageSize - 7, Length: 2*PageSize + 20}

			object, err := client.Get(ctx, Request{Key: Key{3}}, options)
			if err != nil {
				t.Fatal(err)
			}
			defer closeQuietly(object)

			// Split every credit into one-byte writes so Rust must retain partial
			// 12-byte control frames across reads rather than assume one recv.
			object.conn.Conn = &fragmentedReleaseConn{Conn: object.conn.Conn}

			n, err := interopCopy(mode, &offsetSink{offset: options.Offset}, object)
			if err != nil || n != options.Length {
				t.Fatal("partial read", n, err)
			}
		})
	}

	for _, closeObject := range []bool{false, true} {
		t.Run("cancel-held-credit/close="+strconv.FormatBool(closeObject), func(t *testing.T) {
			requestCtx, stop := context.WithCancel(ctx)
			defer stop()

			object, err := client.Get(requestCtx, Request{Key: Key{5}})
			if err != nil {
				t.Fatal(err)
			}
			defer closeQuietly(object)

			// Read part of the first page, then stall with its credit held.
			buffer := make([]byte, 4096)
			if _, err := io.ReadFull(object, buffer); err != nil {
				t.Fatal(err)
			}

			if _, err := (&offsetSink{}).Write(buffer); err != nil {
				t.Fatal(err)
			}

			if closeObject {
				closeQuietly(object)
			} else {
				stop()
			}

			_, err = io.Copy(io.Discard, readerOnly{object})
			if err == nil || !closeObject && !errors.Is(err, context.Canceled) || closeObject && !errors.Is(err, net.ErrClosed) {
				t.Fatal("wrong cancellation result", err)
			}

			// MaxConnections=1 also verifies canceled admission is returned.
			next, err := client.Get(ctx, Request{Key: Key{1}})
			if err != nil {
				t.Fatal("request after cancellation", err)
			}
			defer closeQuietly(next)

			if n, err := io.Copy(&offsetSink{}, next); err != nil || n != 4096 {
				t.Fatal("read after cancellation", n, err)
			}
		})
	}

	t.Run("Get/writer-failure", func(t *testing.T) {
		sentinel := errors.New("destination failure")

		object, err := client.Get(ctx, Request{Key: Key{5}})
		if err != nil {
			t.Fatal(err)
		}
		defer closeQuietly(object)

		_, err = object.WriteTo(writeFunc(func([]byte) (int, error) { return 0, sentinel }))
		if !errors.Is(err, sentinel) {
			t.Fatal(err)
		}

		closeQuietly(object)

		if len(client.bulk.slots) != 0 {
			t.Fatal("failed destination retained admission")
		}
	})
}

// interopCopy drains object through Read or WriteTo, as selected by mode.
func interopCopy(mode string, w io.Writer, object *Object) (int64, error) {
	if mode == "Read" {
		return io.CopyBuffer(w, readerOnly{object}, make([]byte, 32<<10))
	}

	return object.WriteTo(w)
}

type writeFunc func([]byte) (int, error)

func (f writeFunc) Write(p []byte) (int, error) { return f(p) }

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
