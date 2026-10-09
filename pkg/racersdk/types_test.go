// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/Azure/unbounded/pkg/racersdk/internal/wire"
)

func TestKey(t *testing.T) {
	text := strings.Repeat("0123456789abcdef", 4)

	key, err := ParseKey(text)
	if err != nil {
		t.Fatal(err)
	}

	if key.String() != text || key[0] != 0x01 || key[31] != 0xef {
		t.Fatalf("key %x", key)
	}

	for _, bad := range []string{"", text[:62], text + "00", strings.ToUpper(text), "g" + text[1:]} {
		_, err := ParseKey(bad)
		assertIs(t, err, ErrInvalidRequest)
	}
}

func TestFormatRedacts(t *testing.T) {
	r := Request{Key: Key{0xab}, Metadata: "secret-meta", Authorization: "secret-auth"}
	o := OriginRequest{Request: r, ETag: `"v"`, Offset: PageSize, Length: 10}

	for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
		for _, value := range []any{r, &r, o} {
			text := fmt.Sprintf(verb, value)
			if strings.Contains(text, "secret") || !strings.Contains(text, "ab00") {
				t.Errorf("%s of %T = %s", verb, value, text)
			}
		}
	}

	if text := fmt.Sprint(o); !strings.Contains(text, `ETag: "\"v\""`) || !strings.Contains(text, "Offset: 16777216") {
		t.Errorf("origin request = %s", text)
	}
}

func TestMetadataConversion(t *testing.T) {
	expires := time.Date(2026, 1, 2, 3, 4, 5, 123456789, time.FixedZone("x", 3600))
	m := Metadata{Size: 5, ETag: `"v"`, ContentType: "text/plain", ExpiresAt: expires}

	w := m.wire()
	if err := w.Validate(); err != nil {
		t.Fatal(err)
	}

	back := fromWireMetadata(w)
	if back.Size != 5 || back.ETag != `"v"` || back.ContentType != "text/plain" ||
		!back.ExpiresAt.Equal(expires.Truncate(time.Millisecond)) || back.ExpiresAt.Location() != time.UTC {
		t.Fatalf("metadata %+v", back)
	}

	for _, bad := range []Metadata{
		{Size: -1, ETag: `"v"`, ExpiresAt: expires},
		{Size: 1, ETag: "v", ExpiresAt: expires},
		{Size: 1, ETag: `"v"`},
	} {
		if bad.wire().Validate() == nil {
			t.Errorf("metadata %+v accepted", bad)
		}
	}
}

func TestErrorMapping(t *testing.T) {
	for kind, want := range map[wire.ErrorKind]error{
		wire.ErrorInvalidArgument:    ErrInvalidRequest,
		wire.ErrorHeaderLimit:        ErrInvalidRequest,
		wire.ErrorUnauthorized:       ErrUnauthorized,
		wire.ErrorForbidden:          ErrForbidden,
		wire.ErrorNotFound:           ErrNotFound,
		wire.ErrorVersionUnavailable: ErrVersionMismatch,
		wire.ErrorUnsatisfiableRange: ErrRangeNotSatisfiable,
		wire.ErrorUnavailable:        ErrUnavailable,
		wire.ErrorIO:                 ErrUnavailable,
		wire.ErrorProtocol:           nil,
		wire.ErrorInternal:           nil,
		wire.ErrorBadGateway:         nil,
	} {
		err := ioFailure("get", &wire.Error{Kind: kind, Operation: "response", Status: 1})
		if want != nil {
			assertIs(t, err, want)
		} else {
			assertNoSentinel(t, err)
		}

		if !strings.HasPrefix(err.Error(), "racersdk: response: ") {
			t.Errorf("message %q", err)
		}
	}

	cause := errors.New("cause")
	err := ioFailure("get", cause)
	assertIs(t, err, ErrUnavailable)
	assertIs(t, err, cause)

	assertIs(t, ioFailure("get", context.Canceled), context.Canceled)
	assertIs(t, ioFailure("get", context.DeadlineExceeded), context.DeadlineExceeded)
	assertIs(t, closedError("get"), net.ErrClosed)

	if ioFailure("get", nil) != nil || ioFailure("get", io.EOF) != io.EOF {
		t.Fatal("ioFailure must pass nil and io.EOF through")
	}

	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(errClosed)
	assertIs(t, contextError("get", ctx), net.ErrClosed)

	ctx, stop := context.WithCancel(context.Background())
	stop()
	assertIs(t, contextError("get", ctx), context.Canceled)
}
