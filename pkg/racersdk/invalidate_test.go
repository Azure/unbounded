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
	"time"
)

func TestInvalidateSendsDeleteWithIfMatch(t *testing.T) {
	heads := make(chan string, 1)
	c := rawServer(t, func(conn net.Conn, _ *bufio.Reader, head []byte) {
		heads <- string(head)

		_, _ = io.WriteString(conn, "HTTP/1.1 204 No Content\r\nConnection: close\r\n\r\n")
	})

	var key Key

	key[31] = 1

	if err := c.Invalidate(t.Context(), key, `"v1"`); err != nil {
		t.Fatal(err)
	}

	want := "DELETE /v1/objects/" + key.String() + " HTTP/1.1\r\nHost: racer\r\nIf-Match: \"v1\"\r\nContent-Length: 0\r\n\r\n"
	if got := <-heads; got != want {
		t.Fatalf("head = %q; want %q", got, want)
	}
}

func TestInvalidateRejectsWeakTagsWithoutContactingRacer(t *testing.T) {
	c := rawServer(t, func(net.Conn, *bufio.Reader, []byte) {
		t.Error("an invalid ETag reached Racer")
	})

	for _, etag := range []string{"", "v1", `W/"v1"`, "*"} {
		assertIs(t, c.Invalidate(t.Context(), Key{}, etag), ErrInvalidRequest)
	}
}

func TestInvalidateMapsErrorStatuses(t *testing.T) {
	for _, tc := range []struct {
		response string
		want     error
	}{
		{"HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\nConnection: close\r\n\r\n", ErrInvalidRequest},
		{"HTTP/1.1 503 Service Unavailable\r\nContent-Length: 0\r\nConnection: close\r\n\r\n", ErrUnavailable},
	} {
		c := rawServer(t, func(conn net.Conn, _ *bufio.Reader, _ []byte) {
			_, _ = io.WriteString(conn, tc.response)
		})
		assertIs(t, c.Invalidate(t.Context(), Key{}, `"v"`), tc.want)
	}
}

func TestInvalidateRejectsMalformedResponses(t *testing.T) {
	for _, response := range []string{
		"HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n",
		"HTTP/1.1 204 No Content\r\nContent-Length: 5\r\n\r\n",
		"garbage\r\n\r\n",
	} {
		c := rawServer(t, func(conn net.Conn, _ *bufio.Reader, _ []byte) {
			_, _ = io.WriteString(conn, response)
		})

		err := c.Invalidate(t.Context(), Key{}, `"v"`)
		if err == nil || errors.Is(err, ErrInvalidRequest) || errors.Is(err, ErrUnavailable) {
			t.Errorf("response %q: err = %v; want a protocol error", strings.SplitN(response, "\r\n", 2)[0], err)
		}
	}
}

func TestInvalidateHonorsContextCancellation(t *testing.T) {
	c := rawServer(t, func(conn net.Conn, r *bufio.Reader, _ []byte) {
		// Never answer; wait for the client to give up.
		_, _ = r.ReadByte()
	})

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	err := c.Invalidate(ctx, Key{}, `"v"`)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Invalidate = %v; want deadline exceeded", err)
	}
}
