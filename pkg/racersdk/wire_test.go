// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"testing"

	"github.com/Azure/unbounded/pkg/racersdk/internal/originsock"
	"github.com/Azure/unbounded/pkg/racersdk/internal/wire"
)

// Raw fixtures shared by root integration tests deliberately bypass validation.
func rawRequest(method, fields string) []byte {
	return []byte(method + " " + objectPrefix + (Key{}).String() + " HTTP/1.1\r\nHost: racer\r\n" + fields + "\r\n")
}

func rawResponse(status int, fields string) []byte {
	return []byte("HTTP/1.1 " + strconv.Itoa(status) + " " + http.StatusText(status) + "\r\n" + fields + "\r\n")
}

type finalErrorReader struct{ err error }

func (r finalErrorReader) Read(p []byte) (int, error) { return copy(p, "abc"), r.err }

func TestWireErrorMapping(t *testing.T) {
	cause := errors.New("private cause")

	for _, tt := range []struct {
		wire wire.ErrorKind
		sdk  ErrorKind
	}{
		{wire.ErrorInvalidArgument, ErrorInvalidArgument},
		{wire.ErrorProtocol, ErrorProtocol},
		{wire.ErrorUnauthorized, ErrorUnauthorized},
		{wire.ErrorForbidden, ErrorForbidden},
		{wire.ErrorNotFound, ErrorNotFound},
		{wire.ErrorVersionUnavailable, ErrorVersionUnavailable},
		{wire.ErrorUnsatisfiableRange, ErrorUnsatisfiableRange},
		{wire.ErrorHeaderLimit, ErrorHeaderLimit},
		{wire.ErrorInternal, ErrorInternal},
		{wire.ErrorBadGateway, ErrorBadGateway},
		{wire.ErrorUnavailable, ErrorUnavailable},
		{wire.ErrorCanceled, ErrorCanceled},
		{wire.ErrorDeadline, ErrorDeadline},
		{wire.ErrorIO, ErrorIO},
	} {
		for _, status := range []int{0, 503} {
			err := fromWireError(&wire.Error{Kind: tt.wire, Operation: "response", Status: status, Err: cause})
			assertKind(t, err, tt.sdk)

			want := "racersdk response: " + tt.sdk.String()
			if status != 0 {
				want += " (HTTP 503)"
			}

			if err.Error() != want || !errors.Is(err, cause) {
				t.Fatal("changed diagnostic or cause", err)
			}

			if fmt.Sprintf("%#v", err) != want {
				t.Fatal("unsafe diagnostic")
			}
		}
	}

	for _, err := range []error{nil, io.EOF, cause} {
		if fromWireError(err) != err {
			t.Fatal("changed non-wire error")
		}
	}

	nested := fromWireError(&wire.Error{Kind: wire.ErrorBadGateway, Operation: "origin metadata", Err: &wire.Error{Kind: wire.ErrorInvalidArgument, Operation: "metadata"}})
	assertKind(t, errors.Unwrap(nested), ErrorInvalidArgument)

	for _, invalid := range []bool{false, true} {
		err := socketError(&originsock.Error{Invalid: invalid, Operation: "socket path", Cause: cause})

		kind := ErrorIO
		if invalid {
			kind = ErrorInvalidArgument
		}

		assertKind(t, err, kind)

		if err.Error() != "racersdk socket path: "+kind.String() || !errors.Is(err, cause) {
			t.Fatal("changed socket error", err)
		}
	}
}
