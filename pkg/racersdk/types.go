// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package racersdk streams immutable objects through Racer and serves origin
// callbacks over bounded Unix-socket connections.
package racersdk

import (
	"bufio"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Azure/unbounded/pkg/racersdk/internal/wire"
)

const (
	maxHeadBytes    = wire.MaxHeadBytes
	maxFieldBytes   = wire.MaxFieldBytes
	socketPathLimit = 107
)

// Key identifies an object. Every value, including zero, is valid.
type Key [32]byte

// ParseKey parses a 64-character hexadecimal object key.
func ParseKey(s string) (Key, error) {
	key, err := wire.ParseKey(s)
	return Key(key), fromWireError(err)
}

// String returns the lowercase hexadecimal key.
func (k Key) String() string { return hex.EncodeToString(k[:]) }

// ByteLength and ByteOffset are unsigned at the API boundary, but wire values
// must fit MaxInt64. Constructors and Metadata.Validate check that constraint.
type (
	ByteLength uint64
	ByteOffset uint64
)

// CacheName is a DNS subdomain that fits both canonical Linux Unix socket paths.
// Its zero value is invalid.
type CacheName struct{ value string }

// ParseCacheName validates a cache name and its canonical socket path lengths.
func ParseCacheName(s string) (CacheName, error) {
	if len(s) == 0 || len(s) > 253 || len("/run/racer/"+s+"/origin/socket") > socketPathLimit {
		return CacheName{}, failure(ErrorInvalidArgument, "cache name", nil)
	}

	for _, label := range strings.Split(s, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return CacheName{}, failure(ErrorInvalidArgument, "cache name", nil)
		}

		for i := range len(label) {
			c := label[i]
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
				return CacheName{}, failure(ErrorInvalidArgument, "cache name", nil)
			}
		}
	}

	return CacheName{value: s}, nil
}

// String returns the validated cache name.
func (n CacheName) String() string { return n.value }

// ETag is one strong quoted entity tag. Zero means no pin and is invalid metadata.
// Quotes are retained; commas and backslashes within them are literal bytes.
type ETag struct{ value string }

// ParseETag validates a strong quoted entity tag without normalizing its bytes.
func ParseETag(s string) (ETag, error) {
	if err := wire.ValidateETag(s); err != nil {
		return ETag{}, fromWireError(err)
	}

	return ETag{value: s}, nil
}

// String returns the quoted entity tag, or empty for an absent pin.
func (e ETag) String() string { return e.value }

// AdapterMetadata is opaque upstream context. Zero means absent.
type (
	AdapterMetadata struct{ value string }
	// Authorization is an opaque upstream credential. Zero means absent. It has no
	// String or serialization method that exposes the credential.
	Authorization struct{ value string }
)

func validateOpaque(s string) error {
	return fromWireError(wire.ValidateOpaque(s))
}

// ParseAdapterMetadata validates an opaque origin metadata field.
func ParseAdapterMetadata(s string) (AdapterMetadata, error) {
	if err := validateOpaque(s); err != nil {
		return AdapterMetadata{}, err
	}

	return AdapterMetadata{value: s}, nil
}

// ParseAuthorization validates an opaque origin credential field.
func ParseAuthorization(s string) (Authorization, error) {
	if err := validateOpaque(s); err != nil {
		return Authorization{}, err
	}

	return Authorization{value: s}, nil
}

// ForOrigin explicitly exposes the metadata to an origin adapter.
func (m AdapterMetadata) ForOrigin() string { return m.value }

// ForOrigin explicitly exposes the credential to an origin adapter.
func (a Authorization) ForOrigin() string { return a.value }

// Format redacts metadata from diagnostic output.
func (m AdapterMetadata) Format(s fmt.State, _ rune) {
	writeDiagnostic(s, "AdapterMetadata([redacted])")
}

// Format redacts credentials from diagnostic output.
func (a Authorization) Format(s fmt.State, _ rune) { writeDiagnostic(s, "Authorization([redacted])") }

// FetchContext is an immutable pair of optional origin fields. Zero is absent.
type FetchContext struct {
	metadata      AdapterMetadata
	authorization Authorization
}

// NewFetchContext validates and combines optional origin metadata and credentials.
func NewFetchContext(m AdapterMetadata, a Authorization) (FetchContext, error) {
	if m.value != "" {
		if err := validateOpaque(m.value); err != nil {
			return FetchContext{}, err
		}
	}

	if a.value != "" {
		if err := validateOpaque(a.value); err != nil {
			return FetchContext{}, err
		}
	}

	return FetchContext{metadata: m, authorization: a}, nil
}

// Metadata returns the optional origin metadata.
func (c FetchContext) Metadata() AdapterMetadata { return c.metadata }

// Authorization returns the optional origin credential.
func (c FetchContext) Authorization() Authorization { return c.authorization }

// Format redacts origin fields from diagnostic output.
func (c FetchContext) Format(s fmt.State, _ rune) { writeDiagnostic(s, "FetchContext([redacted])") }

// Request identifies an object and its optional origin fetch context.
type Request struct {
	// Key identifies the immutable object to resolve.
	Key Key
	// Context carries optional opaque origin fields.
	Context FetchContext
}

// Format redacts request data from diagnostic output.
func (r Request) Format(s fmt.State, _ rune) { writeDiagnostic(s, "Request([redacted])") }

// ReadOptions selects a subscription byte range. Length zero reads through EOF.
// Pin optionally selects an existing immutable version; without a snapshot,
// zero selects fresh metadata in the same subscription request.
// A range extending beyond the object is rejected rather than silently shortened.
type ReadOptions struct {
	// PageCredits and ByteCredits bound outstanding subscription leases. Zero
	// selects the configured PageWindow (default two) and PageCredits*PageSize
	// bytes. Ordered requests ascending pages; Get always enables it.
	PageCredits int
	// ByteCredits bounds bytes held by outstanding page leases.
	ByteCredits ByteLength
	// Ordered requests ascending page delivery.
	Ordered bool
	// SmallObject selects reserved admission for objects no larger than PageSize.
	// Larger objects fail with ErrorInvalidArgument before a Value is exposed.
	SmallObject bool
	// Offset selects the first object byte, defaulting to zero.
	Offset ByteOffset
	// Length selects a byte count; zero selects through EOF.
	Length ByteLength
	// Pin optionally selects an immutable version.
	Pin ETag
	// Metadata is an optional trusted snapshot for this request's object, usually
	// from Stat. It pins the subscription to its ETag. A nonzero Pin must match.
	// The SDK validates and copies it; do not mutate it during the opening call.
	Metadata *Metadata
}

// Metadata describes the entire immutable version, even for a partial response.
// ExpiresAt is an admission hint, not a deadline for an admitted stream.
type Metadata struct {
	// Size is the full immutable object's byte length.
	Size ByteLength
	// ETag identifies the immutable version.
	ETag ETag
	// ExpiresAt is an admission hint, not an active stream deadline.
	ExpiresAt time.Time
	// ContentType is optional original-object MIME metadata, carried separately
	// from the wire body's application/octet-stream Content-Type.
	ContentType string
}

// Validate rejects invalid size, absent tags, pre-epoch or overflowing expiry,
// and sub-millisecond precision without silently rounding opaque metadata.
func (m Metadata) Validate() error {
	return fromWireError(m.wire().Validate())
}

func validateContentType(s string) error {
	return fromWireError(wire.ValidateContentType(s))
}

// Operation is an origin callback operation. Zero is invalid.
type Operation uint8

const (
	// OperationHead requests full-object metadata without a body.
	OperationHead Operation = iota + 1
	// OperationBootstrap selects a version and its first page.
	OperationBootstrap
	// OperationPinned requests a page of a previously selected version.
	OperationPinned
)

// OriginRequest is constructed only after validating the wire request. The zero
// value is invalid. Range is still unresolved until callback metadata is known.
type OriginRequest struct {
	key       Key
	context   FetchContext
	operation Operation
	pin       ETag
	byteRange Range
}

// Key returns the requested object key.
func (r OriginRequest) Key() Key { return r.key }

// Context returns the origin fetch context.
func (r OriginRequest) Context() FetchContext { return r.context }

// Operation returns the validated callback operation.
func (r OriginRequest) Operation() Operation { return r.operation }

// Pin returns the selected version and whether it is present.
func (r OriginRequest) Pin() (ETag, bool) { return r.pin, r.pin.value != "" }

// Range returns the unresolved inclusive range and whether it is present.
func (r OriginRequest) Range() (Range, bool) { return r.byteRange, r.byteRange.present }

// Format redacts callback request data from diagnostic output.
func (r OriginRequest) Format(s fmt.State, _ rune) { writeDiagnostic(s, "OriginRequest([redacted])") }

// ErrorKind classifies failures without exposing request or callback data.
// Its zero value is unspecified and is not an origin error kind.
type ErrorKind uint8

const (
	// ErrorInvalidArgument indicates invalid caller input.
	ErrorInvalidArgument ErrorKind = iota + 1
	// ErrorClosed indicates a closed client or value.
	ErrorClosed
	// ErrorProtocol indicates malformed wire data.
	ErrorProtocol
	// ErrorUnauthorized indicates missing or invalid authentication.
	ErrorUnauthorized
	// ErrorForbidden indicates access was denied.
	ErrorForbidden
	// ErrorNotFound indicates an unknown object.
	ErrorNotFound
	// ErrorVersionUnavailable indicates the pinned version is unavailable.
	ErrorVersionUnavailable
	// ErrorUnsatisfiableRange indicates a range outside the selected object.
	ErrorUnsatisfiableRange
	// ErrorHeaderLimit indicates a head or field exceeded its limit.
	ErrorHeaderLimit
	// ErrorInternal indicates an internal failure.
	ErrorInternal
	// ErrorBadGateway indicates an invalid upstream response.
	ErrorBadGateway
	// ErrorUnavailable indicates temporary unavailability or exhausted capacity.
	ErrorUnavailable
	// ErrorCanceled indicates context cancellation.
	ErrorCanceled
	// ErrorDeadline indicates an expired context or admission deadline.
	ErrorDeadline
	// ErrorIO indicates a transport or body I/O failure.
	ErrorIO
)

// String returns a safe, human-readable classification.
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

// Kind returns the failure classification, or zero for a nil error.
func (e *Error) Kind() ErrorKind {
	if e == nil {
		return 0
	}

	return e.kind
}

// Operation returns the safe operation name, or empty for a nil error.
func (e *Error) Operation() string {
	if e == nil {
		return ""
	}

	return e.operation
}

// StatusCode returns the HTTP status, or zero when unavailable.
func (e *Error) StatusCode() int {
	if e == nil {
		return 0
	}

	return e.status
}

// Unwrap exposes the original cause for explicit inspection.
func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}

	return e.cause
}

// Error returns a diagnostic that does not expose request or callback data.
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
	err := fromWireError(wire.StatusError(status))

	var typed *Error
	errors.As(err, &typed)

	return typed
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

// PageSize is the v1 whole-page origin transfer unit.
const PageSize ByteLength = wire.PageSize

// Range is an inclusive closed byte range. Its zero value is absent.
// Origin callbacks receive whole-page ranges; client continuations are private.
type Range struct {
	present     bool
	first, last uint64
}

// Bounds returns the inclusive, unresolved wire bounds and whether a range is
// present. Origin adapters should use Resolve to validate against object size.
func (r Range) Bounds() (first, last ByteOffset, present bool) {
	return ByteOffset(r.first), ByteOffset(r.last), r.present
}

// ClosedRange validates and constructs an inclusive byte range.
func ClosedRange(first, last ByteOffset) (Range, error) {
	r, err := wire.ClosedRange(uint64(first), uint64(last))
	return fromWireRange(r), fromWireError(err)
}

func (r Range) resolve(size ByteLength) (ByteOffset, ByteOffset, error) {
	first, last, err := r.wire().Resolve(uint64(size))
	return ByteOffset(first), ByteOffset(last), fromWireError(err)
}

// Resolve validates a whole origin page and returns its inclusive bounds in the
// selected immutable version. The final page is shortened at EOF. An absent range,
// unaligned start, or partial nonfinal page is invalid; an empty object is unsatisfiable.
func (r Range) Resolve(size ByteLength) (ByteOffset, ByteOffset, error) {
	first, last, err := r.wire().ResolvePage(uint64(size))
	return ByteOffset(first), ByteOffset(last), fromWireError(err)
}

const (
	objectPrefix       = wire.ObjectPrefix
	clientObjectPrefix = wire.ClientObjectPrefix
)

// fromWireError is the sole translation from protocol classifications to public SDK errors.
// Translate nested protocol causes too, preserving the public error chain.
func fromWireError(err error) error {
	var e *wire.Error
	if !errors.As(err, &e) {
		return err
	}

	var kind ErrorKind

	switch e.Kind {
	case wire.ErrorInvalidArgument:
		kind = ErrorInvalidArgument
	case wire.ErrorProtocol:
		kind = ErrorProtocol
	case wire.ErrorUnauthorized:
		kind = ErrorUnauthorized
	case wire.ErrorForbidden:
		kind = ErrorForbidden
	case wire.ErrorNotFound:
		kind = ErrorNotFound
	case wire.ErrorVersionUnavailable:
		kind = ErrorVersionUnavailable
	case wire.ErrorUnsatisfiableRange:
		kind = ErrorUnsatisfiableRange
	case wire.ErrorHeaderLimit:
		kind = ErrorHeaderLimit
	case wire.ErrorInternal:
		kind = ErrorInternal
	case wire.ErrorBadGateway:
		kind = ErrorBadGateway
	case wire.ErrorUnavailable:
		kind = ErrorUnavailable
	case wire.ErrorCanceled:
		kind = ErrorCanceled
	case wire.ErrorDeadline:
		kind = ErrorDeadline
	case wire.ErrorIO:
		kind = ErrorIO
	default:
		kind = ErrorInternal
	}

	return &Error{kind: kind, operation: e.Operation, status: e.Status, cause: fromWireError(e.Err)}
}

func (r Range) wire() wire.Range { return wire.Range{Present: r.present, First: r.first, Last: r.last} }
func fromWireRange(r wire.Range) Range {
	return Range{present: r.Present, first: r.First, last: r.Last}
}

func (m Metadata) wire() wire.Metadata {
	return wire.Metadata{Size: uint64(m.Size), ETag: m.ETag.value, ExpiresAt: m.ExpiresAt, ContentType: m.ContentType}
}

func fromWireMetadata(m wire.Metadata) Metadata {
	return Metadata{Size: ByteLength(m.Size), ETag: ETag{value: m.ETag}, ExpiresAt: m.ExpiresAt, ContentType: m.ContentType}
}

func (r OriginRequest) wire() wire.Request {
	return wire.Request{Key: r.key, Operation: wire.Operation(r.operation), Pin: r.pin.value, Range: r.byteRange.wire(), AdapterMetadata: r.context.metadata.value, Authorization: r.context.authorization.value}
}

func fromWireRequest(r wire.Request) OriginRequest {
	return OriginRequest{key: r.Key, operation: Operation(r.Operation), pin: ETag{value: r.Pin}, byteRange: fromWireRange(r.Range), context: FetchContext{metadata: AdapterMetadata{value: r.AdapterMetadata}, authorization: Authorization{value: r.Authorization}}}
}

type wireResponse struct {
	metadata    Metadata
	first, last ByteOffset
	length      int64
	close       bool
}

func fromWireResponse(r wire.Response) wireResponse {
	return wireResponse{metadata: fromWireMetadata(r.Metadata), first: ByteOffset(r.First), last: ByteOffset(r.Last), length: r.Length, close: r.Close}
}
func decimal(s string) (uint64, error) { n, err := wire.Decimal(s); return n, fromWireError(err) }
func parseRange(s string) (Range, error) {
	r, err := wire.ParseRange(s)
	return fromWireRange(r), fromWireError(err)
}
func bootstrapRange() Range           { return fromWireRange(wire.BootstrapRange()) }
func validatePageShape(r Range) error { return fromWireError(wire.ValidatePageShape(r.wire())) }
func (o ReadOptions) wire() wire.SubscriptionOptions {
	r := wire.SubscriptionOptions{Offset: uint64(o.Offset), Length: uint64(o.Length), PageCredits: o.PageCredits, ByteCredits: uint64(o.ByteCredits), Ordered: o.Ordered, SmallObject: o.SmallObject, Pin: o.Pin.value}
	if o.Metadata != nil {
		m := o.Metadata.wire()
		r.Metadata = &m
	}

	return r
}
func validateRequest(r OriginRequest) error { return fromWireError(wire.ValidateRequest(r.wire())) }
func requestHead(r OriginRequest) ([]byte, error) {
	b, err := wire.RequestHead(r.wire())
	return b, fromWireError(err)
}

func clientHead(r OriginRequest) ([]byte, error) {
	b, err := wire.ClientHead(r.wire())
	return b, fromWireError(err)
}

func parseRequestHead(b []byte, origin bool) (OriginRequest, error) {
	r, err := wire.ParseRequestHead(b, origin)
	return fromWireRequest(r), fromWireError(err)
}

func parseResponseHead(b []byte, r OriginRequest, snapshot *Metadata) (wireResponse, error) {
	var m *wire.Metadata

	if snapshot != nil {
		copy := snapshot.wire()
		m = &copy
	}

	response, err := wire.ParseResponseHead(b, r.wire(), m)

	return fromWireResponse(response), fromWireError(err)
}

func originResponse(r OriginRequest, m Metadata) (wireResponse, error) {
	response, err := wire.OriginResponse(r.wire(), m.wire())
	return fromWireResponse(response), fromWireError(err)
}

func metadataHeaders(m Metadata) (http.Header, error) {
	h, err := wire.MetadataHeaders(m.wire())
	return h, fromWireError(err)
}

func contentRangeValue(first, last ByteOffset, size ByteLength) (string, error) {
	s, err := wire.ContentRangeValue(uint64(first), uint64(last), uint64(size))
	return s, fromWireError(err)
}

func readRawHead(r *bufio.Reader, response bool) ([]byte, error) {
	b, err := wire.ReadRawHead(r, response)
	return b, fromWireError(err)
}

func readHeadBytes(r *bufio.Reader, response bool) ([]byte, error) {
	b, err := wire.ReadHeadBytes(r, response)
	return b, fromWireError(err)
}

func headHeaders(b []byte) http.Header   { return wire.HeadHeaders(b) }
func connectionClose(h http.Header) bool { return wire.ConnectionClose(h) }
