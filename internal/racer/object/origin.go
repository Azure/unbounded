// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package object adapts S3 objects to racersdk: an Origin that reads
// exact object versions from S3 and a sidecar HTTP handler that serves them
// to pods through a racersdk client.
package object

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"reflect"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"

	"github.com/Azure/unbounded/pkg/racersdk"
)

// Object identifies an exact upstream object within a configured namespace.
type Object struct {
	Schema    int    `json:"schema"`
	Namespace string `json:"namespace"`
	Bucket    string `json:"bucket"`
	Key       string `json:"key"`
	VersionID string `json:"versionId,omitempty"`
}

// NewRequest hashes canonical schema-1 JSON and carries it as origin metadata.
// Keys and version IDs are never cleaned, decoded, or normalized.
func NewRequest(namespace, bucket, key, versionID string) (racersdk.Request, error) {
	object := Object{Schema: 1, Namespace: namespace, Bucket: bucket, Key: key, VersionID: versionID}
	if !validObject(object) {
		return racersdk.Request{}, racersdk.ErrInvalidRequest
	}

	canonical, err := json.Marshal(object)
	if err != nil {
		return racersdk.Request{}, fmt.Errorf("%w: %w", racersdk.ErrInvalidRequest, err)
	}

	// JSON leaves DEL literal, but SDK metadata rejects it. Escape it before hashing.
	canonical = bytes.ReplaceAll(canonical, []byte{0x7f}, []byte(`\u007f`))

	if len(canonical) > 8<<10 {
		return racersdk.Request{}, racersdk.ErrInvalidRequest
	}

	return racersdk.Request{Key: sha256.Sum256(canonical), Metadata: string(canonical)}, nil
}

func validNamespace(value string) bool {
	return value != "" && len(value) <= 256 && utf8.ValidString(value) &&
		strings.IndexFunc(value, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) == -1
}

func validBucket(value string) bool {
	if value == "" || len(value) > 255 || value == "." || value == ".." {
		return false
	}

	// Include legacy S3-compatible bucket names, but not paths or endpoint URLs.
	for _, c := range value {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '.' && c != '-' && c != '_' {
			return false
		}
	}

	return true
}

func validObject(object Object) bool {
	return object.Schema == 1 && validNamespace(object.Namespace) && validBucket(object.Bucket) &&
		object.Key != "" && len(object.Key) <= 1024 && utf8.ValidString(object.Key) &&
		len(object.VersionID) <= 1024 && utf8.ValidString(object.VersionID)
}

var (
	errInvalidNamespace = errors.New("invalid namespace")
	errInvalidBucket    = errors.New("invalid bucket")
)

// scope binds requests to one namespace and an optional bucket allowlist.
// An empty allowlist admits every bucket.
type scope struct {
	namespace string
	buckets   map[string]struct{}
}

// newScope validates and copies the configured namespace and buckets.
func newScope(namespace string, buckets []string) (scope, error) {
	if !validNamespace(namespace) {
		return scope{}, errInvalidNamespace
	}

	s := scope{namespace: namespace, buckets: make(map[string]struct{}, len(buckets))}
	for _, bucket := range buckets {
		if !validBucket(bucket) {
			return scope{}, errInvalidBucket
		}

		s.buckets[bucket] = struct{}{}
	}

	return s, nil
}

func (s scope) allows(bucket string) bool {
	if len(s.buckets) == 0 {
		return true
	}

	_, ok := s.buckets[bucket]

	return ok
}

// decodeObject checks both the canonical bytes and their hash so alternate JSON
// spellings cannot give the same upstream object a different cache identity.
func decodeObject(request racersdk.OriginRequest, s scope) (Object, error) {
	raw := request.Metadata

	var object Object
	if err := json.Unmarshal([]byte(raw), &object); err != nil || !validObject(object) {
		return Object{}, racersdk.ErrInvalidRequest
	}

	canonical, err := NewRequest(object.Namespace, object.Bucket, object.Key, object.VersionID)
	if err != nil || canonical.Metadata != raw || canonical.Key != request.Key {
		return Object{}, racersdk.ErrInvalidRequest
	}

	if object.Namespace != s.namespace || !s.allows(object.Bucket) {
		return Object{}, racersdk.ErrForbidden
	}

	return object, nil
}

// S3Client is the read-only subset of the AWS v2 S3 client used by the origin.
type S3Client interface {
	HeadObject(context.Context, *s3.HeadObjectInput, ...func(*s3.Options)) (*s3.HeadObjectOutput, error)
	GetObject(context.Context, *s3.GetObjectInput, ...func(*s3.Options)) (*s3.GetObjectOutput, error)
}

// OriginConfig binds requests to one upstream namespace and optional buckets.
type OriginConfig struct {
	Namespace string
	Buckets   []string
	// MetadataTTL sets when Racer must stop serving this version from cache.
	// Zero expires metadata immediately.
	MetadataTTL time.Duration
}

// NewOrigin serves full metadata and conditional page reads without caching.
// The client must return bodies whose Close interrupts Read, as the AWS client does.
func NewOrigin(client S3Client, config OriginConfig) (racersdk.Origin, error) {
	if client == nil || config.MetadataTTL < 0 {
		return nil, racersdk.ErrInvalidRequest
	}

	value := reflect.ValueOf(client)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Map, reflect.Pointer, reflect.Slice:
		if value.IsNil() {
			return nil, racersdk.ErrInvalidRequest
		}
	}

	bound, err := newScope(config.Namespace, config.Buckets)
	if err != nil {
		return nil, racersdk.ErrInvalidRequest
	}

	origin := &objectOrigin{client: client, scope: bound, ttl: config.MetadataTTL}

	return origin.read, nil
}

type objectOrigin struct {
	client S3Client
	scope  scope
	ttl    time.Duration
}

func (o *objectOrigin) read(ctx context.Context, request racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return racersdk.Metadata{}, nil, classifyS3Error(err, false)
	}

	object, err := decodeObject(request, o.scope)
	if err != nil {
		return racersdk.Metadata{}, nil, err
	}

	input := objectHeadInput(request, object)

	head, metadata, err := o.readHead(ctx, request, object, input)
	if err != nil {
		return metadata, nil, err
	}

	if request.Head {
		return metadata, nil, nil
	}

	body, err := o.readPage(ctx, request, input, head, metadata)

	return metadata, body, err
}

// readHead rechecks the pin on every page, including upstreams that ignore If-Match.
func (o *objectOrigin) readHead(ctx context.Context, request racersdk.OriginRequest, object Object, input *s3.HeadObjectInput) (*s3.HeadObjectOutput, racersdk.Metadata, error) {
	pin, pinned := request.ETag, request.ETag != ""

	head, err := o.client.HeadObject(ctx, input)
	if err != nil {
		return nil, racersdk.Metadata{}, classifyS3Error(err, pinned)
	}

	metadata, err := o.headMetadata(head, object)
	if err != nil {
		return nil, racersdk.Metadata{}, err
	}

	if pinned && metadata.ETag != pin {
		return head, metadata, racersdk.ErrVersionMismatch
	}

	return head, metadata, nil
}

// objectHeadInput omits unspecified versions and pins rather than sending empty ones.
func objectHeadInput(request racersdk.OriginRequest, object Object) *s3.HeadObjectInput {
	input := &s3.HeadObjectInput{Bucket: aws.String(object.Bucket), Key: aws.String(object.Key)}
	if object.VersionID != "" {
		input.VersionId = aws.String(object.VersionID)
	}

	if request.ETag != "" {
		input.IfMatch = aws.String(request.ETag)
	}

	return input
}

// readPage resolves the range against the fresh HEAD and pins GET to that version.
func (o *objectOrigin) readPage(ctx context.Context, request racersdk.OriginRequest, input *s3.HeadObjectInput, head *s3.HeadObjectOutput, metadata racersdk.Metadata) (io.ReadCloser, error) {
	if request.Offset < 0 || request.Offset%racersdk.PageSize != 0 || request.Length <= 0 || request.Length > racersdk.PageSize {
		return nil, racersdk.ErrInvalidRequest
	}

	// The SDK handles empty objects and pages outside the object.
	if request.Offset >= metadata.Size {
		return nil, nil
	}

	first := request.Offset
	last := first + min(request.Length, metadata.Size-first) - 1

	// Pin GET to the version HEAD saw, so an overwrite with identical bytes
	// (same ETag, new version) between HEAD and GET cannot fail the version
	// comparison in validateObjectPage. Unversioned buckets return no version
	// ID from HEAD, so none is sent.
	version := input.VersionId
	if version == nil && aws.ToString(head.VersionId) != "" {
		version = head.VersionId
	}

	get := &s3.GetObjectInput{
		Bucket: input.Bucket, Key: input.Key, VersionId: version,
		IfMatch: aws.String(metadata.ETag), Range: aws.String(fmt.Sprintf("bytes=%d-%d", first, last)),
	}

	output, err := o.client.GetObject(ctx, get)
	if err != nil {
		return responseBody(output), unpinnedRace(request, classifyS3Error(err, true))
	}

	// Transfer even a rejected body to the SDK, which closes it exactly once.
	// Do not limit the reader: the SDK must detect short and excess bodies.
	return responseBody(output), unpinnedRace(request, validateObjectPage(output, head, metadata, first, last))
}

// unpinnedRace reports a HEAD/GET mismatch as retryable when Racer did not pin
// an ETag. The ETag GET fails to match came from this origin's own HEAD, so
// ErrVersionMismatch (412 to Racer) would name a version the caller never asked
// for. The object changed underneath the read; a retry observes the new version.
func unpinnedRace(request racersdk.OriginRequest, err error) error {
	if request.ETag != "" || !errors.Is(err, racersdk.ErrVersionMismatch) {
		return err
	}

	// Keep only the upstream cause: wrapping err itself would leave
	// ErrVersionMismatch reachable through errors.Is.
	var upstream privateS3Error
	if errors.As(err, &upstream) {
		return fmt.Errorf("%w: object changed during read: %w", racersdk.ErrUnavailable, upstream)
	}

	return fmt.Errorf("%w: object changed during read", racersdk.ErrUnavailable)
}

func (o *objectOrigin) headMetadata(head *s3.HeadObjectOutput, object Object) (racersdk.Metadata, error) {
	if head == nil || head.ContentLength == nil || *head.ContentLength < 0 || aws.ToBool(head.DeleteMarker) ||
		aws.ToString(head.ContentRange) != "" || (object.VersionID != "" && aws.ToString(head.VersionId) != object.VersionID) {
		return racersdk.Metadata{}, errS3Validation
	}

	// SDK metadata cannot carry Content-Encoding to the reader.
	if encoding := aws.ToString(head.ContentEncoding); encoding != "" && !strings.EqualFold(encoding, "identity") {
		return racersdk.Metadata{}, errS3Validation
	}

	tag := aws.ToString(head.ETag)
	if !validS3ETag(tag) {
		return racersdk.Metadata{}, errS3Validation
	}

	metadata := racersdk.Metadata{
		Size: *head.ContentLength, ETag: tag,
		ExpiresAt: time.Now().Add(o.ttl).Truncate(time.Millisecond), ContentType: normalizeS3ContentType(aws.ToString(head.ContentType)),
	}

	return metadata, nil
}

var errInvalidS3Response = errors.New("racer-object: invalid S3 response")

// normalizeS3ContentType follows racersdk/internal/wire.ValidateContentType.
// Unsupported values become absent metadata so the sidecar can use its fallback.
func normalizeS3ContentType(value string) string {
	if len(value) > 256 || strings.TrimSpace(value) != value {
		return ""
	}

	for i := range len(value) {
		if value[i] < 0x20 || value[i] > 0x7e {
			return ""
		}
	}

	rest := value
	token := func() string {
		i := 0
		for i < len(rest) && (rest[i] >= 'a' && rest[i] <= 'z' || rest[i] >= 'A' && rest[i] <= 'Z' ||
			rest[i] >= '0' && rest[i] <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", rune(rest[i]))) {
			i++
		}

		part := rest[:i]
		rest = rest[i:]

		return part
	}

	consume := func(b byte) bool {
		if len(rest) == 0 || rest[0] != b {
			return false
		}

		rest = rest[1:]

		return true
	}
	if token() == "" || !consume('/') || token() == "" {
		return ""
	}

	var parameters []string

	for rest != "" {
		rest = strings.TrimLeft(rest, " ")

		if !consume(';') {
			return ""
		}

		rest = strings.TrimLeft(rest, " ")

		name := token()
		if name == "" {
			return ""
		}

		for _, old := range parameters {
			if strings.EqualFold(old, name) {
				return ""
			}
		}

		parameters = append(parameters, name)
		rest = strings.TrimLeft(rest, " ")

		if !consume('=') {
			return ""
		}

		rest = strings.TrimLeft(rest, " ")

		if consume('"') {
			for {
				if rest == "" {
					return ""
				}

				b := rest[0]
				rest = rest[1:]

				if b == '"' {
					break
				}

				if b == '\\' {
					if rest == "" {
						return ""
					}

					rest = rest[1:]
				}
			}
		} else if token() == "" {
			return ""
		}
	}

	return value
}

func validS3ETag(tag string) bool {
	if len(tag) < 2 || len(tag) > 8<<10 || tag[0] != '"' || tag[len(tag)-1] != '"' {
		return false
	}

	for i := 1; i < len(tag)-1; i++ {
		if tag[i] != 0x21 && (tag[i] < 0x23 || tag[i] > 0x7e) {
			return false
		}
	}

	return true
}

func responseBody(output *s3.GetObjectOutput) io.ReadCloser {
	if output == nil {
		return nil
	}

	return output.Body
}

func validateObjectPage(output *s3.GetObjectOutput, head *s3.HeadObjectOutput, metadata racersdk.Metadata, first, last int64) error {
	if output == nil {
		return errS3Validation
	}

	tag := aws.ToString(output.ETag)
	if !validS3ETag(tag) {
		return errS3Validation
	}

	if tag != metadata.ETag {
		return racersdk.ErrVersionMismatch
	}

	// Compare raw content types so normalization cannot hide a HEAD/GET mismatch.
	if output.Body == nil || output.ContentLength == nil || *output.ContentLength != int64(last-first+1) ||
		aws.ToString(output.ContentRange) != fmt.Sprintf("bytes %d-%d/%d", first, last, metadata.Size) ||
		aws.ToString(output.ContentType) != aws.ToString(head.ContentType) || aws.ToString(output.VersionId) != aws.ToString(head.VersionId) ||
		!strings.EqualFold(aws.ToString(output.ContentEncoding), aws.ToString(head.ContentEncoding)) || aws.ToBool(output.DeleteMarker) {
		return errS3Validation
	}

	return nil
}

func classifyS3Error(err error, pinned bool) error {
	classification := s3Error(err)

	// A missing pinned object means the selected version is no longer available.
	if pinned && classification == racersdk.ErrNotFound {
		classification = racersdk.ErrVersionMismatch
	}

	return fmt.Errorf("%w: %w", classification, privateS3Error{err})
}

// Keep upstream details out of messages while preserving the cause for errors.Is.
type privateS3Error struct{ cause error }

func (e privateS3Error) Error() string { return "S3 request failed" }
func (e privateS3Error) Unwrap() error { return e.cause }

// s3Error prefers cancellation and known API codes over HTTP status codes.
func s3Error(err error) error {
	var (
		api    smithy.APIError
		status interface{ HTTPStatusCode() int }
	)

	switch {
	case errors.Is(err, context.Canceled):
		return context.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		return context.DeadlineExceeded
	case errors.As(err, &api):
		if classification := s3APIError(api.ErrorCode()); classification != errInvalidS3Response {
			return classification
		}
	}

	// The AWS SDK wraps send failures in a synthetic response with status 0.
	// Only a real HTTP status takes precedence over the transport cause.
	if errors.As(err, &status) && status.HTTPStatusCode() != 0 {
		return s3HTTPError(status.HTTPStatusCode())
	}

	var (
		connection interface{ ConnectionError() bool }
		op         *net.OpError
		dns        *net.DNSError
	)

	if (errors.As(err, &connection) && connection.ConnectionError()) || errors.As(err, &op) || errors.As(err, &dns) {
		return racersdk.ErrUnavailable
	}

	return errInvalidS3Response
}

func s3APIError(code string) error {
	switch code {
	// S3 answers a GET or HEAD of a delete marker's version ID with
	// MethodNotAllowed (405), so a delete marker reads as not found.
	case "NoSuchKey", "NoSuchBucket", "NoSuchVersion", "NotFound", "MethodNotAllowed":
		return racersdk.ErrNotFound
	case "AccessDenied", "InvalidAccessKeyId", "SignatureDoesNotMatch", "ExpiredToken", "InvalidToken", "TokenRefreshRequired":
		return racersdk.ErrForbidden
	case "PreconditionFailed":
		return racersdk.ErrVersionMismatch
	case "SlowDown", "ServiceUnavailable", "InternalError", "RequestTimeout", "Throttling", "ThrottlingException":
		return racersdk.ErrUnavailable
	default:
		return errInvalidS3Response
	}
}

func s3HTTPError(code int) error {
	switch {
	case code == 401:
		return racersdk.ErrUnauthorized
	case code == 403:
		return racersdk.ErrForbidden
	case code == 404 || code == 405:
		// 405 is S3's answer for a delete marker requested by version ID.
		return racersdk.ErrNotFound
	case code == 412:
		return racersdk.ErrVersionMismatch
	case code == 408 || code == 429 || code >= 500:
		return racersdk.ErrUnavailable
	default:
		// An upstream 416 contradicts the range resolved from HEAD, not caller input.
		return invalidS3Status(code)
	}
}
