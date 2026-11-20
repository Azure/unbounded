// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
)

func TestPutEarlyResponseBeforeBody(t *testing.T) {
	c := rawServer(t, func(conn net.Conn, _ *bufio.Reader, _ []byte) {
		_, _ = io.WriteString(conn, "HTTP/1.1 503 X\r\nContent-Length: 0\r\nRetry-After: 1\r\nRacer-Write-Error: owners\r\n\r\n")
	})

	size := int64(4 * PageSize)

	_, err := c.Put(context.Background(), Request{Key: Key{1}}, io.LimitReader(zeros{}, size), size, WriteOptions{Upload: UploadNever})
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("%v", err)
	}
}

func TestPutConnectionLost(t *testing.T) {
	c := rawServer(t, func(net.Conn, *bufio.Reader, []byte) {})

	_, err := c.Put(context.Background(), Request{Key: Key{1}}, strings.NewReader("abc"), 3, WriteOptions{Upload: UploadNever})
	if !errors.Is(err, ErrUnavailable) || errors.Is(err, ErrBusy) {
		t.Fatalf("%v", err)
	}
}

func TestPutSuccessAfterSendFailureIsNotTrusted(t *testing.T) {
	// A 201 that arrives before the body was delivered cannot describe this
	// body, so the client reports the send failure instead.
	c := rawServer(t, func(conn net.Conn, _ *bufio.Reader, _ []byte) {
		_, _ = io.WriteString(conn, "HTTP/1.1 201 Created\r\nETag: \"racer-00000000000000000000000000000000\"\r\n"+
			"Racer-Upload-State: never\r\nContent-Length: 0\r\n\r\n")
	})

	size := int64(4 * PageSize)

	_, err := c.Put(context.Background(), Request{Key: Key{1}}, io.LimitReader(zeros{}, size), size, WriteOptions{Upload: UploadNever})
	if err == nil {
		t.Fatal("accepted a 201 for an undelivered body")
	}
}

func TestWriteBadResponse(t *testing.T) {
	c := rawServer(t, func(conn net.Conn, _ *bufio.Reader, _ []byte) {
		_, _ = io.WriteString(conn, "HTTP/1.1 201 Created\r\nContent-Length: 0\r\n\r\n")
	})

	_, err := c.Put(context.Background(), Request{Key: Key{1}}, strings.NewReader("abc"), 3, WriteOptions{Upload: UploadNever})
	if err == nil || errors.Is(err, ErrUnavailable) {
		t.Fatalf("%v", err)
	}
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

func TestWriteErrorSentinels(t *testing.T) {
	cases := map[int]error{
		409: ErrConflict,
		412: ErrExists,
		500: ErrInternal,
		502: ErrUploadFailed,
		503: ErrBusy,
		504: ErrUnavailable,
	}
	for status, want := range cases {
		err := error(&WriteError{Op: "put", Status: status})
		if !errors.Is(err, want) {
			t.Errorf("%d: not %v", status, want)
		}

		if status != 500 && errors.Is(err, ErrInternal) {
			t.Errorf("%d: matches ErrInternal", status)
		}
	}

	if errors.Is(&WriteError{Status: 500}, ErrUnavailable) {
		t.Error("500 matches ErrUnavailable")
	}
}

func TestPutInternalError(t *testing.T) {
	c := rawServer(t, func(conn net.Conn, _ *bufio.Reader, _ []byte) {
		_, _ = io.WriteString(conn, "HTTP/1.1 500 X\r\nContent-Length: 0\r\n\r\n")
	})

	_, err := c.Put(context.Background(), Request{Key: Key{1}}, strings.NewReader("abc"), 3, WriteOptions{Upload: UploadNever})
	if !errors.Is(err, ErrInternal) || errors.Is(err, ErrBusy) {
		t.Fatalf("%v", err)
	}
}
