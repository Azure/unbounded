// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"encoding/hex"
	"fmt"
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

func ParseKey(s string) (Key, error) {
	key, err := wire.ParseKey(s)
	return Key(key), fromWireError(err)
}

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

func (n CacheName) String() string { return n.value }

// ETag is one strong quoted entity tag. Zero means no pin and is invalid metadata.
// Quotes are retained; commas and backslashes within them are literal bytes.
type ETag struct{ value string }

func ParseETag(s string) (ETag, error) {
	if err := wire.ValidateETag(s); err != nil {
		return ETag{}, fromWireError(err)
	}

	return ETag{value: s}, nil
}

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

func ParseAdapterMetadata(s string) (AdapterMetadata, error) {
	if err := validateOpaque(s); err != nil {
		return AdapterMetadata{}, err
	}

	return AdapterMetadata{value: s}, nil
}

func ParseAuthorization(s string) (Authorization, error) {
	if err := validateOpaque(s); err != nil {
		return Authorization{}, err
	}

	return Authorization{value: s}, nil
}

func (m AdapterMetadata) ForOrigin() string { return m.value }
func (a Authorization) ForOrigin() string   { return a.value }
func (m AdapterMetadata) Format(s fmt.State, _ rune) {
	writeDiagnostic(s, "AdapterMetadata([redacted])")
}

func (a Authorization) Format(s fmt.State, _ rune) { writeDiagnostic(s, "Authorization([redacted])") }

// FetchContext is an immutable pair of optional origin fields. Zero is absent.
type FetchContext struct {
	metadata      AdapterMetadata
	authorization Authorization
}

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

func (c FetchContext) Metadata() AdapterMetadata    { return c.metadata }
func (c FetchContext) Authorization() Authorization { return c.authorization }
func (c FetchContext) Format(s fmt.State, _ rune)   { writeDiagnostic(s, "FetchContext([redacted])") }

// Request identifies an object and its optional origin fetch context.
type Request struct {
	Key     Key
	Context FetchContext
}

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
	ByteCredits ByteLength
	Ordered     bool
	// SmallObject selects reserved admission for objects no larger than PageSize.
	// Larger objects fail with ErrorInvalidArgument before a Value is exposed.
	SmallObject bool
	Offset      ByteOffset
	Length      ByteLength
	Pin         ETag
	// Metadata is an optional trusted snapshot for this request's object, usually
	// from Stat. It pins the subscription to its ETag. A nonzero Pin must match.
	// The SDK validates and copies it; do not mutate it during the opening call.
	Metadata *Metadata
}

// Metadata describes the entire immutable version, even for a partial response.
// ExpiresAt is an admission hint, not a deadline for an admitted stream.
type Metadata struct {
	Size      ByteLength
	ETag      ETag
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
	OperationHead Operation = iota + 1
	OperationBootstrap
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

func (r OriginRequest) Key() Key                   { return r.key }
func (r OriginRequest) Context() FetchContext      { return r.context }
func (r OriginRequest) Operation() Operation       { return r.operation }
func (r OriginRequest) Pin() (ETag, bool)          { return r.pin, r.pin.value != "" }
func (r OriginRequest) Range() (Range, bool)       { return r.byteRange, r.byteRange.present }
func (r OriginRequest) Format(s fmt.State, _ rune) { writeDiagnostic(s, "OriginRequest([redacted])") }
