// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/Azure/unbounded/pkg/racersdk/internal/wire"
)

// spliceDestination accepts buffered writes and fails ReadFrom, which WriteTo
// only uses once it can splice straight from the Racer socket.
type spliceDestination struct {
	err error
	// drain reads the source to EOF and fails only if it ends early.
	drain   bool
	spliced bool
}

func (d *spliceDestination) Write(p []byte) (int, error) { return len(p), nil }

func (d *spliceDestination) ReadFrom(r io.Reader) (int64, error) {
	d.spliced = true
	if !d.drain {
		return 0, d.err
	}

	n, err := io.Copy(io.Discard, r)
	if err != nil {
		return n, err
	}

	if limited, ok := r.(*io.LimitedReader); ok && limited.N > 0 {
		return n, d.err
	}

	return n, nil
}

func assertNotIs(t *testing.T, err, target error) {
	t.Helper()

	if errors.Is(err, target) {
		t.Fatalf("error %v matches %v", err, target)
	}
}

func TestObjectWriteToSpliceDestinationFailure(t *testing.T) {
	diskFull := errors.New("disk full")

	for _, tc := range []struct {
		name string
		err  error
		want error
	}{
		{"writer error", diskFull, diskFull},
		{"broken pipe", unix.EPIPE, unix.EPIPE},
		{"reset with healthy source", unix.ECONNRESET, unix.ECONNRESET},
		{"writer EOF", io.EOF, io.EOF},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := fakeClient(t, offsetOrigin(4<<20))

			o, err := c.Get(t.Context(), Request{})
			if err != nil {
				t.Fatal(err)
			}
			defer closeQuietly(o)

			dst := &spliceDestination{err: tc.err}

			_, err = o.WriteTo(dst)
			if !dst.spliced {
				t.Fatal("WriteTo never spliced")
			}

			assertIs(t, err, ErrDestination)
			assertIs(t, err, tc.want)
			assertNotIs(t, err, ErrUnavailable)
			assertNotIs(t, err, io.ErrUnexpectedEOF)

			if n, again := o.WriteTo(io.Discard); n != 0 || again != err {
				t.Fatalf("repeated WriteTo = %d, %v; want 0, %v", n, again, err)
			}

			if got := len(c.bulk.slots); got != 0 {
				t.Fatalf("retained %d admission slots", got)
			}
		})
	}
}

func TestObjectWriteToSpliceAmbiguousTimeout(t *testing.T) {
	c := fakeClient(t, offsetOrigin(4<<20))

	o, err := c.Get(t.Context(), Request{})
	if err != nil {
		t.Fatal(err)
	}
	defer closeQuietly(o)

	dst := &spliceDestination{err: os.ErrDeadlineExceeded}

	_, err = o.WriteTo(dst)
	if !dst.spliced {
		t.Fatal("WriteTo never spliced")
	}

	assertIs(t, err, os.ErrDeadlineExceeded)
	assertNotIs(t, err, ErrDestination)
	assertNotIs(t, err, ErrUnavailable)
}

func TestObjectWriteToSpliceSourceTruncated(t *testing.T) {
	const (
		size = 1 << 20
		sent = size / 2
	)

	c := rawServer(t, func(conn net.Conn, _ *bufio.Reader, _ []byte) {
		if _, err := io.WriteString(conn, subscriptionHead(size, 0, size)); err != nil {
			return
		}

		if err := writeFrame(conn, wire.PageFrame, 0, 0, size); err != nil {
			return
		}

		// Cut the page short, then close.
		_, _ = io.WriteString(conn, strings.Repeat("x", sent))
	})

	o, err := c.Get(t.Context(), Request{})
	if err != nil {
		t.Fatal(err)
	}
	defer closeQuietly(o)

	dst := &spliceDestination{err: unix.ECONNRESET, drain: true}

	n, err := o.WriteTo(dst)
	if !dst.spliced {
		t.Fatal("WriteTo never spliced")
	}

	if n > sent {
		t.Fatalf("WriteTo = %d bytes; source sent only %d", n, sent)
	}

	assertIs(t, err, io.ErrUnexpectedEOF)
	assertIs(t, err, ErrUnavailable)
	assertNotIs(t, err, ErrDestination)
}

func TestObjectWriteToClosedSocketDestination(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}

	f := os.NewFile(uintptr(fds[0]), "destination")

	conn, err := net.FileConn(f)
	closeQuietly(f)

	if err != nil {
		_ = unix.Close(fds[1])

		t.Fatal(err)
	}

	defer closeQuietly(conn)

	if err := unix.Close(fds[1]); err != nil {
		t.Fatal(err)
	}

	c := fakeClient(t, offsetOrigin(4<<20))

	o, err := c.Get(t.Context(), Request{})
	if err != nil {
		t.Fatal(err)
	}
	defer closeQuietly(o)

	_, err = o.WriteTo(conn)
	assertIs(t, err, ErrDestination)
	assertIs(t, err, unix.EPIPE)
	assertNotIs(t, err, ErrUnavailable)
	assertNotIs(t, err, io.ErrUnexpectedEOF)
}

func TestDestinationFailureWrapping(t *testing.T) {
	cause := errors.New("cause")

	err := destinationFailure(cause)
	assertIs(t, err, ErrDestination)
	assertIs(t, err, cause)

	if destinationFailure(err) != err {
		t.Fatal("destinationFailure wrapped twice")
	}

	if destinationFailure(nil) != nil {
		t.Fatal("destinationFailure(nil) != nil")
	}

	if ioFailure("write", err) != err {
		t.Fatal("ioFailure reclassified a destination failure")
	}

	if !strings.Contains(err.Error(), "destination failed: cause") {
		t.Fatalf("message %q", err)
	}

	for _, sentinel := range []error{ErrDestination, fmt.Errorf("proxy: %w", ErrDestination)} {
		err := destinationFailure(sentinel)
		if err == sentinel {
			t.Fatalf("%v passed through without the private marker", sentinel)
		}

		got := ioFailure("write", err)
		assertIs(t, got, ErrDestination)
		assertIs(t, got, sentinel)
		assertNotIs(t, got, ErrUnavailable)
	}
}

// TestObjectWriteToWriterReturnsErrDestination covers a writer that itself
// returns ErrDestination, for example a nested WriteTo. The result must still
// match only ErrDestination.
func TestObjectWriteToWriterReturnsErrDestination(t *testing.T) {
	for name, cause := range map[string]error{
		"direct":  ErrDestination,
		"wrapped": fmt.Errorf("proxy: %w", ErrDestination),
	} {
		t.Run(name, func(t *testing.T) {
			c := fakeClient(t, offsetOrigin(1000))

			o, err := c.Get(t.Context(), Request{})
			if err != nil {
				t.Fatal(err)
			}
			defer closeQuietly(o)

			_, err = o.WriteTo(writeFunc(func([]byte) (int, error) { return 0, cause }))
			assertIs(t, err, ErrDestination)
			assertIs(t, err, cause)
			assertNotIs(t, err, ErrUnavailable)
		})
	}
}
