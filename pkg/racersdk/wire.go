// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"bufio"
	"errors"
	"net/http"

	"github.com/Azure/unbounded/pkg/racersdk/internal/wire"
)

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
func bootstrapRange() Range              { return fromWireRange(wire.BootstrapRange()) }
func nominalPageEnd(first uint64) uint64 { return wire.NominalPageEnd(first) }
func validatePageShape(r Range) error    { return fromWireError(wire.ValidatePageShape(r.wire())) }
func (o ReadOptions) wire() wire.SubscriptionOptions {
	r := wire.SubscriptionOptions{Offset: uint64(o.Offset), Length: uint64(o.Length), PageCredits: o.PageCredits, ByteCredits: uint64(o.ByteCredits), Ordered: o.Ordered, SmallObject: o.SmallObject, Pin: o.Pin.value}
	if o.Metadata != nil {
		m := o.Metadata.wire()
		r.Metadata = &m
	}

	return r
}
func validateRequest(r OriginRequest) error      { return fromWireError(wire.ValidateRequest(r.wire())) }
func requestHeaders(r OriginRequest) http.Header { return wire.RequestHeaders(r.wire()) }
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
