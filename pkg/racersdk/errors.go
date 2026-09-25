// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"context"
	"errors"
	"fmt"
	"io"
)

// ErrorKind classifies failures without exposing request or callback data.
// Its zero value is unspecified and is not an origin error kind.
type ErrorKind uint8

const (
	ErrorInvalidArgument ErrorKind = iota + 1
	ErrorClosed
	ErrorProtocol
	ErrorUnauthorized
	ErrorForbidden
	ErrorNotFound
	ErrorVersionUnavailable
	ErrorUnsatisfiableRange
	ErrorHeaderLimit
	ErrorInternal
	ErrorBadGateway
	ErrorUnavailable
	ErrorCanceled
	ErrorDeadline
	ErrorIO
)

func (k ErrorKind) String() string {
	switch k {
	case ErrorInvalidArgument:
		return "invalid argument"
	case ErrorClosed:
		return "closed"
	case ErrorProtocol:
		return "protocol"
	case ErrorUnauthorized:
		return "unauthorized"
	case ErrorForbidden:
		return "forbidden"
	case ErrorNotFound:
		return "not found"
	case ErrorVersionUnavailable:
		return "version unavailable"
	case ErrorUnsatisfiableRange:
		return "unsatisfiable range"
	case ErrorHeaderLimit:
		return "header limit"
	case ErrorInternal:
		return "internal"
	case ErrorBadGateway:
		return "bad gateway"
	case ErrorUnavailable:
		return "unavailable"
	case ErrorCanceled:
		return "canceled"
	case ErrorDeadline:
		return "deadline"
	case ErrorIO:
		return "I/O"
	default:
		return "unspecified"
	}
}

// Error carries a safe operation name, classification, and optional HTTP status.
// Unwrap deliberately exposes the original cause for explicit inspection only.
// The zero value is an unspecified failure. Fields are private to prevent unsafe
// operation strings from entering diagnostics.
type Error struct {
	kind      ErrorKind
	operation string
	status    int
	cause     error
}

func (e *Error) Kind() ErrorKind {
	if e == nil {
		return 0
	}

	return e.kind
}

func (e *Error) Operation() string {
	if e == nil {
		return ""
	}

	return e.operation
}

func (e *Error) StatusCode() int {
	if e == nil {
		return 0
	}

	return e.status
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}

	return e.cause
}

func (e *Error) Error() string {
	if e == nil {
		return "racersdk: unspecified"
	}

	if e.status != 0 {
		return fmt.Sprintf("racersdk %s: %s (HTTP %d)", e.operation, e.kind, e.status)
	}

	return "racersdk " + e.operation + ": " + e.kind.String()
}

// Format also redacts Go-syntax formatting (%#v), which otherwise reveals causes.
func (e Error) Format(s fmt.State, _ rune) { writeDiagnostic(s, e.Error()) }

func failure(kind ErrorKind, op string, cause error) *Error {
	return &Error{kind: kind, operation: op, cause: cause}
}

func writeDiagnostic(w io.Writer, text string) {
	// fmt.State cannot usefully report a writer error back through Format.
	if _, err := io.WriteString(w, text); err != nil {
		return
	}
}

// NewOriginError wraps a callback failure. Supported kinds map to 400, 401, 403,
// 404, 412, 416, 431, 500, 502, and 503 respectively: InvalidArgument,
// Unauthorized, Forbidden, NotFound, VersionUnavailable, UnsatisfiableRange,
// HeaderLimit, Internal, BadGateway, and Unavailable. Canceled and Deadline also
// map to 503. Unsupported kinds become Internal. A 416 requires valid callback
// Metadata; the serving layer must check it before constructing the response.
func NewOriginError(kind ErrorKind, cause error) error {
	if originStatus(kind) == 0 {
		kind = ErrorInternal
	}

	return failure(kind, "origin", cause)
}

func originStatus(kind ErrorKind) int {
	switch kind {
	case ErrorInvalidArgument:
		return 400
	case ErrorUnauthorized:
		return 401
	case ErrorForbidden:
		return 403
	case ErrorNotFound:
		return 404
	case ErrorVersionUnavailable:
		return 412
	case ErrorUnsatisfiableRange:
		return 416
	case ErrorHeaderLimit:
		return 431
	case ErrorInternal:
		return 500
	case ErrorBadGateway:
		return 502
	case ErrorUnavailable, ErrorCanceled, ErrorDeadline:
		return 503
	default:
		return 0
	}
}

// callbackStatus handles semantic errors only; the server owns body closure,
// validation of 416 metadata, and the before/after-headers decision.
func callbackStatus(err error, pinned bool) int {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return 503
	}

	var sdkErr *Error
	if errors.As(err, &sdkErr) {
		status := originStatus(sdkErr.Kind())
		if status == 404 && pinned {
			return 412
		}

		if status != 0 {
			return status
		}
	}

	return 500
}

func statusError(status int) *Error {
	kind := ErrorProtocol

	switch status {
	case 400, 405:
		kind = ErrorInvalidArgument
	case 401:
		kind = ErrorUnauthorized
	case 403:
		kind = ErrorForbidden
	case 404:
		kind = ErrorNotFound
	case 412:
		kind = ErrorVersionUnavailable
	case 416:
		kind = ErrorUnsatisfiableRange
	case 431:
		kind = ErrorHeaderLimit
	case 500:
		kind = ErrorInternal
	case 502:
		kind = ErrorBadGateway
	case 503:
		kind = ErrorUnavailable
	}

	return &Error{kind: kind, operation: "response", status: status}
}

func ioFailure(op string, err error) error {
	if err == nil || err == io.EOF {
		return err
	}

	kind := ErrorIO
	if errors.Is(err, context.Canceled) {
		kind = ErrorCanceled
	}

	if errors.Is(err, context.DeadlineExceeded) {
		kind = ErrorDeadline
	}

	return failure(kind, op, err)
}
