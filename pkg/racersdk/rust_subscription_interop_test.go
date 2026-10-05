// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

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

	t.Run("DownloadTo/partial-and-empty", func(t *testing.T) {
		sink := &interopWriterAt{seen: make(map[int64]bool)}
		options := ReadOptions{Offset: ByteOffset(PageSize - 7), Length: PageSize + 20, PageCredits: 1, ByteCredits: PageSize}

		n, err := client.DownloadTo(ctx, Request{Key: Key{3}}, sink, options)
		if err != nil || n != int64(options.Length) || len(sink.seen) != 3 || !sink.seen[int64(options.Offset)] {
			t.Fatal("partial absolute-offset download", n, err, sink.seen)
		}

		empty := &interopWriterAt{seen: make(map[int64]bool)}
		if n, err := client.DownloadTo(ctx, Request{}, empty); n != 0 || err != nil || len(empty.seen) != 0 {
			t.Fatal("empty download", n, err)
		}
	})

	t.Run("DownloadTo/large-bounded", func(t *testing.T) {
		sink := &interopWriterAt{seen: make(map[int64]bool)}

		n, err := client.DownloadTo(ctx, Request{Key: Key{4}}, sink, ReadOptions{PageCredits: 2, ByteCredits: 2 * PageSize})
		if err != nil || n != 32*int64(PageSize)+13 || len(sink.seen) != 33 {
			t.Fatalf("download bytes=%d pages=%d: %v", n, len(sink.seen), err)
		}

		t.Logf("verified %d bytes with two SDK page credits and 64 MiB Rust plaintext limit", n)
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

	t.Run("DownloadTo/writer-failure", func(t *testing.T) {
		sentinel := errors.New("destination failure")

		n, err := client.DownloadTo(ctx, Request{Key: Key{5}}, interopFailWriter{sentinel})
		if n != 0 || !errors.Is(err, sentinel) {
			t.Fatal(n, err)
		}
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

// No object-sized backing buffer; validate absolute offsets and each byte.
type interopWriterAt struct{ seen map[int64]bool }

func (w *interopWriterAt) WriteAt(p []byte, offset int64) (int, error) {
	if w.seen[offset] {
		return 0, fmt.Errorf("duplicate offset %d", offset)
	}

	w.seen[offset] = true

	return (&offsetSink{offset: offset}).Write(p)
}

type interopFailWriter struct{ err error }

func (w interopFailWriter) WriteAt([]byte, int64) (int, error) { return 0, w.err }
