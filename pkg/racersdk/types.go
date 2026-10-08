// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"strconv"
	"time"

	"github.com/Azure/unbounded/pkg/racersdk/internal/wire"
)

// PageSize is the unit in which Racer caches and transfers objects (16 MiB).
// Objects no larger than PageSize may be read with [ReadOptions.SmallObject].
const PageSize = 16 << 20

// The public constant must match the wire protocol.
var _ [PageSize - wire.PageSize]struct{}

var _ [wire.PageSize - PageSize]struct{}

// Key names an object in a Racer cache. The object's content may change
// over time; each version is identified by the ETag in its [Metadata] and
// must never change. A key is often a content digest, such as the SHA-256
// of a blob, in which case the object has only one version.
type Key [32]byte

// ParseKey parses 64 lowercase hexadecimal characters.
func ParseKey(s string) (Key, error) {
	key, err := wire.ParseKey(s)
	if err != nil {
		return Key{}, invalid("key", err)
	}

	return key, nil
}

// String returns the 64-character lowercase hexadecimal form of k.
func (k Key) String() string { return hex.EncodeToString(k[:]) }

// Request identifies an object and carries the opaque values that Racer
// forwards unchanged to the [Origin] on a cache miss.
//
// Metadata and Authorization may be empty; otherwise, each is limited to 8 KiB
// with no leading or trailing ASCII space (0x20), bytes below 0x20, or byte 0x7f.
// Bytes above 0x7f are allowed; values need not be valid UTF-8.
// Formatting a Request with the fmt package redacts both values.
type Request struct {
	Key Key
	// Metadata tells the origin how to locate the object, for example a bucket
	// and path. Racer does not interpret it.
	Metadata string
	// Authorization is a credential for the origin. Racer does not interpret it.
	Authorization string
}

// Format prints the key and redacts Metadata and Authorization.
func (r Request) Format(s fmt.State, _ rune) {
	// fmt.State cannot usefully report a write error back through Format.
	_, _ = io.WriteString(s, "Request{Key: "+r.Key.String()+", Metadata: [redacted], Authorization: [redacted]}") //nolint:errcheck // See above.
}

func (r Request) wire(op wire.Operation) (wire.Request, error) {
	w := wire.Request{Key: r.Key, Operation: op, AdapterMetadata: r.Metadata, Authorization: r.Authorization}
	for _, value := range []string{r.Metadata, r.Authorization} {
		if value != "" {
			if err := wire.ValidateOpaque(value); err != nil {
				return w, invalid("request", err)
			}
		}
	}

	return w, nil
}

// Metadata describes one version of an object. The bytes of a version,
// identified by its ETag, must never change.
type Metadata struct {
	// Size is the object length in bytes.
	Size int64
	// ETag is a strong HTTP entity tag such as "\"v1\"". Racer uses it to
	// ensure every page of a read comes from the same version.
	ETag string
	// ContentType is optional.
	ContentType string
	// ExpiresAt is when Racer must stop serving this version from cache. It
	// is required and kept at millisecond precision.
	ExpiresAt time.Time
}

func (m Metadata) wire() wire.Metadata {
	size := uint64(m.Size)
	if m.Size < 0 {
		size = math.MaxUint64
	}

	expiresAt := m.ExpiresAt
	sec := expiresAt.Unix()
	ms := int64(expiresAt.Nanosecond() / int(time.Millisecond))
	// Keep invalid timestamps for wire validation rather than wrapping UnixMilli.
	if sec >= 0 && sec <= math.MaxInt64/1000 && (sec < math.MaxInt64/1000 || ms <= math.MaxInt64%1000) {
		expiresAt = time.UnixMilli(expiresAt.UnixMilli()).UTC()
	}

	return wire.Metadata{Size: size, ETag: m.ETag, ExpiresAt: expiresAt, ContentType: m.ContentType}
}

func fromWireMetadata(m wire.Metadata) Metadata {
	return Metadata{Size: int64(m.Size), ETag: m.ETag, ExpiresAt: m.ExpiresAt, ContentType: m.ContentType}
}

// ReadOptions selects part of an object or a specific version. The zero value
// reads the whole current version.
type ReadOptions struct {
	// Offset is the first byte to read.
	Offset int64
	// Length is the number of bytes to read. Zero reads to the end of the
	// object. A range that extends past the end fails with
	// [ErrRangeNotSatisfiable].
	Length int64
	// ETag pins the read to one version, typically from [Client.Stat]. Use it
	// when several reads of a key must see the same bytes, since separate
	// unpinned reads may see different versions. If that version is no
	// longer available, Get fails with [ErrVersionMismatch].
	ETag string
	// SmallObject declares that the object is at most [PageSize] bytes. Small
	// reads use a separate admission queue so they are not delayed behind
	// large transfers. Get fails with [ErrInvalidRequest] if the object is
	// larger.
	SmallObject bool
}

// Errors returned by [Client] methods and [Object] reads, and, except for
// [ErrDestination], recognized when returned by an [Origin]. Test for them
// with [errors.Is].
//
// Other failures wrap the cause: [context.Canceled] or
// [context.DeadlineExceeded] when a context ends, and [net.ErrClosed] after
// [Client.Close] or [Object.Close]. When [Object.WriteTo] times out and
// cannot tell whether Racer or the destination stalled, the error wraps
// [os.ErrDeadlineExceeded] and matches neither [ErrUnavailable] nor
// [ErrDestination]. Anything else is a protocol or origin failure that
// callers usually report as a bad gateway.
var (
	// ErrInvalidRequest reports an invalid key, request value, option, or
	// configuration.
	ErrInvalidRequest = errors.New("racersdk: invalid request")
	// ErrUnauthorized reports that the origin rejected or required credentials.
	ErrUnauthorized = errors.New("racersdk: unauthorized")
	// ErrForbidden reports that the origin denied access.
	ErrForbidden = errors.New("racersdk: forbidden")
	// ErrNotFound reports that the object does not exist.
	ErrNotFound = errors.New("racersdk: not found")
	// ErrVersionMismatch reports that the requested ETag is no longer
	// available.
	ErrVersionMismatch = errors.New("racersdk: version mismatch")
	// ErrRangeNotSatisfiable reports a range outside the object.
	ErrRangeNotSatisfiable = errors.New("racersdk: range not satisfiable")
	// ErrUnavailable reports a temporary failure: Racer or the origin is
	// overloaded or unreachable, or a transfer was cut short. Retrying may
	// succeed.
	ErrUnavailable = errors.New("racersdk: unavailable")
	// ErrDestination reports that the writer passed to [Object.WriteTo]
	// failed, for example because a downstream client disconnected, a disk
	// is full, or a write timed out. Racer and the origin are not at fault,
	// and retrying the read does not help unless the destination recovers.
	// The error also wraps the writer's own error.
	ErrDestination = errors.New("racersdk: destination failed")
)

// destinationError marks a failure of the writer passed to WriteTo, so it is
// never mistaken for a Racer transport failure.
type destinationError struct{ err error }

func (e *destinationError) Error() string {
	return "racersdk: write: destination failed: " + e.err.Error()
}

func (e *destinationError) Unwrap() error { return e.err }

func (e *destinationError) Is(target error) bool { return target == ErrDestination }

// destinationFailure attributes err to the WriteTo destination. Nil and
// errors already carrying the private marker pass through unchanged. A writer
// error that merely matches ErrDestination is still wrapped, because only the
// marker keeps ioFailure from reclassifying it.
func destinationFailure(err error) error {
	var dst *destinationError
	if err == nil || errors.As(err, &dst) {
		return err
	}

	return &destinationError{err: err}
}

// sdkError carries a private classification that maps onto at most one
// exported sentinel, plus a safe operation name and the underlying cause.
type sdkError struct {
	kind   wire.ErrorKind
	op     string
	status int
	err    error
}

func (e *sdkError) Error() string {
	msg := "racersdk: " + e.op + ": " + kindText(e.kind)
	if e.status != 0 {
		msg += " (HTTP " + strconv.Itoa(e.status) + ")"
	}

	if e.err != nil {
		msg += ": " + e.err.Error()
	}

	return msg
}

func (e *sdkError) Unwrap() error { return e.err }

func (e *sdkError) Is(target error) bool {
	return target != nil && sentinel(e.kind) == target
}

func sentinel(kind wire.ErrorKind) error {
	switch kind {
	case wire.ErrorInvalidArgument, wire.ErrorHeaderLimit:
		return ErrInvalidRequest
	case wire.ErrorUnauthorized:
		return ErrUnauthorized
	case wire.ErrorForbidden:
		return ErrForbidden
	case wire.ErrorNotFound:
		return ErrNotFound
	case wire.ErrorVersionUnavailable:
		return ErrVersionMismatch
	case wire.ErrorUnsatisfiableRange:
		return ErrRangeNotSatisfiable
	case wire.ErrorUnavailable, wire.ErrorIO:
		return ErrUnavailable
	default:
		return nil
	}
}

func kindText(kind wire.ErrorKind) string {
	switch kind {
	case wire.ErrorInvalidArgument:
		return "invalid request"
	case wire.ErrorClosed:
		return "closed"
	case wire.ErrorProtocol:
		return "protocol violation"
	case wire.ErrorUnauthorized:
		return "unauthorized"
	case wire.ErrorForbidden:
		return "forbidden"
	case wire.ErrorNotFound:
		return "not found"
	case wire.ErrorVersionUnavailable:
		return "version mismatch"
	case wire.ErrorUnsatisfiableRange:
		return "range not satisfiable"
	case wire.ErrorHeaderLimit:
		return "header limit exceeded"
	case wire.ErrorInternal:
		return "internal error"
	case wire.ErrorBadGateway:
		return "bad gateway"
	case wire.ErrorUnavailable:
		return "unavailable"
	case wire.ErrorCanceled:
		return "canceled"
	case wire.ErrorDeadline:
		return "deadline exceeded"
	default:
		return "I/O failure"
	}
}

func failure(kind wire.ErrorKind, op string, err error) error {
	return &sdkError{kind: kind, op: op, err: err}
}

func invalid(op string, err error) error {
	return failure(wire.ErrorInvalidArgument, op, unwrapWire(err))
}

var errClosed = fmt.Errorf("closed: %w", net.ErrClosed)

func closedError(op string) error {
	return failure(wire.ErrorClosed, op, net.ErrClosed)
}

// ioFailure classifies a transport error, preserving SDK errors, destination
// errors, and context errors. Nil and io.EOF pass through unchanged.
func ioFailure(op string, err error) error {
	if err == nil || err == io.EOF {
		return err
	}

	var dst *destinationError
	if errors.As(err, &dst) {
		return err
	}

	var typed *sdkError
	if errors.As(err, &typed) {
		return err
	}

	var w *wire.Error
	if errors.As(err, &w) {
		return &sdkError{kind: w.Kind, op: w.Operation, status: w.Status, err: w.Err}
	}

	kind := wire.ErrorIO

	switch {
	case errors.Is(err, context.Canceled):
		kind = wire.ErrorCanceled
	case errors.Is(err, context.DeadlineExceeded):
		kind = wire.ErrorDeadline
	}

	return failure(kind, op, err)
}

// unwrapWire drops a wire classification, keeping only its cause, so callers
// can apply their own.
func unwrapWire(err error) error {
	var w *wire.Error
	if errors.As(err, &w) {
		return w.Err
	}

	return err
}

// contextError reports why ctx ended: client closure or the caller's context.
func contextError(op string, ctx context.Context) error {
	if errors.Is(context.Cause(ctx), errClosed) {
		return closedError(op)
	}

	return ioFailure(op, ctx.Err())
}
