// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package s3 is the cachestore driver for in-DC S3-compatible stores.
// In production this targets VAST or another S3-compatible object
// store; in dev it targets a self-hosted S3-compatible store (Garage).
//
// Commit uses stat-then-put: HeadObject the chunk path, and if it is
// already present skip the upload and report ErrCommitLost (another
// replica won the fill race). This needs only GET/PUT/HEAD, so it works
// against any S3-compatible backend, not just those that implement the
// If-None-Match: * conditional-write precondition. Correctness rests on
// the content-addressed layout: the ETag is part of the chunk path, so
// two writers racing on the same key always carry byte-identical bytes
// (see designs/orca/design.md s8). The boot SelfTest verifies
// read-after-write visibility; the versioning gate refuses versioned
// buckets, where retaining every overwrite of an immutable chunk would
// only waste space.
package s3

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/Azure/unbounded/internal/orca/cachestore"
	"github.com/Azure/unbounded/internal/orca/chunk"
)

// Driver implements cachestore.CacheStore against an S3-compatible
// endpoint.
type Driver struct {
	client *s3.Client
	bucket string
	log    *slog.Logger
}

// Config is the s3-driver configuration. Mirrors config.CachestoreS3
// but kept package-local so the driver can be unit-tested without
// importing the whole config package.
type Config struct {
	Endpoint     string
	Bucket       string
	Region       string
	AccessKey    string
	SecretKey    string
	UsePathStyle bool
}

// New constructs a Driver. The bucket-versioning gate is run here
// unconditionally: a versioned bucket retains every overwrite, and
// because Orca's chunks are immutable and only ever re-committed with
// byte-identical content, versioning would accumulate redundant object
// versions that only waste space. The driver refuses to start against
// a versioned bucket.
//
// The log receives debug-level emissions for every chunk operation
// (Get, Put, Stat, Delete) and step-by-step boot trace from
// SelfTest / versioningGate. Passing nil falls back to
// slog.Default().
//
// SelfTest is a separate step (called by main after New)
// to keep the constructor side-effect-light.
func New(ctx context.Context, cfg Config, log *slog.Logger) (*Driver, error) {
	if cfg.Bucket == "" {
		return nil, fmt.Errorf("cachestore/s3: bucket required")
	}

	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("cachestore/s3: endpoint required")
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(cfg.Region),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			cfg.AccessKey, cfg.SecretKey, "",
		)),
		// Opt out of CRC64NVME default introduced in aws-sdk-go-v2
		// 1.32. Several S3-compatible backends reject unknown
		// checksum algorithms with InvalidRequest; real AWS S3 still
		// works either way.
		awsconfig.WithRequestChecksumCalculation(aws.RequestChecksumCalculationWhenRequired),
		awsconfig.WithResponseChecksumValidation(aws.ResponseChecksumValidationWhenRequired),
	)
	if err != nil {
		return nil, fmt.Errorf("cachestore/s3: aws config: %w", err)
	}

	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(cfg.Endpoint)
		o.UsePathStyle = cfg.UsePathStyle
	})

	if log == nil {
		log = slog.Default()
	}

	d := &Driver{
		client: client,
		bucket: cfg.Bucket,
		log:    log,
	}

	if err := d.versioningGate(ctx); err != nil {
		return nil, err
	}

	return d, nil
}

// versioningGate refuses to start if the bucket has versioning enabled
// or suspended. Orca's chunks are immutable and only ever re-committed
// with byte-identical content, so a versioned bucket would retain
// redundant object versions that only waste space.
func (d *Driver) versioningGate(ctx context.Context) error {
	d.log.LogAttrs(ctx, slog.LevelDebug, "versioning_gate_probe",
		slog.String("bucket", d.bucket),
	)

	out, err := d.client.GetBucketVersioning(ctx, &s3.GetBucketVersioningInput{
		Bucket: aws.String(d.bucket),
	})
	if err != nil {
		return fmt.Errorf("cachestore/s3: GetBucketVersioning failed: %w", err)
	}

	d.log.LogAttrs(ctx, slog.LevelDebug, "versioning_gate_status",
		slog.String("bucket", d.bucket),
		slog.String("status", string(out.Status)),
	)

	return validateBucketVersioning(d.bucket, out.Status)
}

// validateBucketVersioning returns an error if the bucket's versioning
// status is incompatible with cachestore/s3. Extracted as a pure
// function so unit tests can cover all branches (empty / Enabled /
// Suspended) without round-tripping to a real or emulated S3 backend.
func validateBucketVersioning(bucket string, status s3types.BucketVersioningStatus) error {
	switch status {
	case s3types.BucketVersioningStatusEnabled, s3types.BucketVersioningStatusSuspended:
		return fmt.Errorf(
			"cachestore/s3: bucket %s has versioning %s; Orca's chunks are "+
				"immutable and re-committed only with byte-identical content, so a "+
				"versioned bucket would accumulate redundant object versions; "+
				"disable bucket versioning to use cachestore/s3",
			bucket, status)
	}

	return nil
}

// SelfTest verifies the backend provides read-after-write visibility,
// which the stat-then-put commit step depends on: a chunk this replica
// (or any peer) just wrote must be observable by a subsequent
// HeadObject so concurrent fills converge instead of all re-uploading.
// It writes a probe key, confirms it is visible via HeadObject, reads
// the bytes back via GetObject, and deletes the probe.
func (d *Driver) SelfTest(ctx context.Context) error {
	suffix, err := randHex(16)
	if err != nil {
		return fmt.Errorf("cachestore/s3 self-test: generate probe key: %w", err)
	}

	probeKey := fmt.Sprintf("_orca-selftest/%s", suffix)
	body := []byte("orca-selftest")

	d.log.LogAttrs(ctx, slog.LevelDebug, "selftest_put",
		slog.String("bucket", d.bucket),
		slog.String("probe_key", probeKey),
	)

	if _, err := d.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(d.bucket),
		Key:    aws.String(probeKey),
		Body:   bytes.NewReader(body),
	}); err != nil {
		return fmt.Errorf("cachestore/s3 self-test: put failed: %w", err)
	}

	// Best-effort cleanup regardless of how the verification below ends.
	defer func() {
		_, _ = d.client.DeleteObject(ctx, &s3.DeleteObjectInput{ //nolint:errcheck // best-effort selftest cleanup
			Bucket: aws.String(d.bucket),
			Key:    aws.String(probeKey),
		})
	}()

	d.log.LogAttrs(ctx, slog.LevelDebug, "selftest_head_expecting_present",
		slog.String("bucket", d.bucket),
		slog.String("probe_key", probeKey),
	)

	// Read-after-write: the object just written must be visible.
	if _, err := d.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(d.bucket),
		Key:    aws.String(probeKey),
	}); err != nil {
		return fmt.Errorf(
			"cachestore/s3: backend does not provide read-after-write visibility; "+
				"refusing to start (HeadObject of a just-written key failed): %w", err)
	}

	out, err := d.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(d.bucket),
		Key:    aws.String(probeKey),
	})
	if err != nil {
		return fmt.Errorf("cachestore/s3 self-test: read-back failed: %w", err)
	}

	got, err := io.ReadAll(out.Body)
	_ = out.Body.Close() //nolint:errcheck // best-effort selftest cleanup

	if err != nil {
		return fmt.Errorf("cachestore/s3 self-test: read-back body: %w", err)
	}

	if !bytes.Equal(got, body) {
		return fmt.Errorf(
			"cachestore/s3: read-after-write returned wrong bytes (got %d, want %d); "+
				"refusing to start", len(got), len(body))
	}

	d.log.LogAttrs(ctx, slog.LevelDebug, "selftest_ok",
		slog.String("bucket", d.bucket),
		slog.String("probe_key", probeKey),
	)

	return nil
}

// GetChunk fetches [off, off+n) of the chunk path from the bucket.
//
// Rejects n <= 0 with a sentinel ErrInvalidArgument: the wire-format
// boundary (cluster.DecodeChunkKey) already rejects object_size <= 0,
// so an in-process caller asking for a zero-length read is a logic
// bug. Forwarding the request would yield a malformed S3 Range
// header (bytes=0--1).
func (d *Driver) GetChunk(ctx context.Context, k chunk.Key, off, n int64) (io.ReadCloser, error) {
	if n <= 0 {
		return nil, fmt.Errorf("cachestore/s3 get: n must be > 0, got %d", n)
	}

	if off < 0 {
		return nil, fmt.Errorf("cachestore/s3 get: off must be >= 0, got %d", off)
	}

	rng := fmt.Sprintf("bytes=%d-%d", off, off+n-1)

	d.log.LogAttrs(ctx, slog.LevelDebug, "cachestore_get_chunk",
		csChunkAttrs(k),
		slog.Int64("off", off),
		slog.Int64("n", n),
	)

	out, err := d.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(d.bucket),
		Key:    aws.String(k.Path()),
		Range:  aws.String(rng),
	})
	if err != nil {
		mapped := mapErr(err)
		d.log.LogAttrs(ctx, slog.LevelDebug, "cachestore_get_chunk_err",
			csChunkAttrs(k),
			slog.Any("err", mapped),
		)

		return nil, mapped
	}

	return out.Body, nil
}

// PutChunk commits the chunk with a stat-then-put step: HeadObject the
// chunk path, and if it is already present skip the upload and return
// ErrCommitLost (another replica won the fill race; the existing object
// is byte-identical because the ETag is part of the path). Otherwise
// upload via plain PutObject.
//
// The HeadObject/PutObject pair has a benign time-of-check/time-of-use
// window: two replicas may both observe the key absent and both upload.
// That degrades to a last-writer-wins overwrite of byte-identical
// content, so readers always observe correct bytes.
//
// Rejects size <= 0 with a sentinel error: a zero-byte chunk is
// never a legitimate fill result (the wire-format boundary already
// rejects object_size <= 0, and the smallest legitimate tail chunk
// is 1 byte), and uploading a zero-byte object would poison the
// path so later GetChunk(n=expected) reads return 0 bytes and break
// the streaming model.
func (d *Driver) PutChunk(ctx context.Context, k chunk.Key, size int64, r io.Reader) error {
	if size <= 0 {
		return fmt.Errorf("cachestore/s3 put: size must be > 0, got %d", size)
	}

	// Validate the body up front (cheap, no RPC) so malformed callers
	// are rejected before any round-trip.
	//
	// AWS SDK v2 needs an io.ReadSeeker for unsigned-payload uploads
	// (so it can rewind on signed-retry). If the caller already passed
	// a seekable reader we hand it to the SDK directly; otherwise
	// buffer the bytes ourselves as a fallback.
	body, ok := r.(io.ReadSeeker)
	if !ok {
		buf, err := io.ReadAll(r)
		if err != nil {
			return fmt.Errorf("cachestore/s3 put: read body: %w", err)
		}
		// Validate the actual byte count against the caller's
		// claimed size.
		if int64(len(buf)) != size {
			return fmt.Errorf("cachestore/s3 put: short body (got %d want %d)", len(buf), size)
		}

		body = bytes.NewReader(buf)
	} else {
		// Seekable-path size validation: probe the reader's length
		// via Seek(0, End), confirm it matches the declared size,
		// then rewind to position 0 for the upload. Without this
		// guard, a buggy caller passing a Reader of length M with
		// size=N would either be rejected by S3 (ContentLength
		// mismatch) or upload a truncated / overlong blob,
		// depending on backend behaviour. The wire-format boundary
		// already rejects size <= 0; this catches the size > 0 but
		// mismatched-bytes case at the driver entry point.
		end, err := body.Seek(0, io.SeekEnd)
		if err != nil {
			return fmt.Errorf("cachestore/s3 put: seek-end: %w", err)
		}

		if end != size {
			return fmt.Errorf("cachestore/s3 put: seekable reader length %d does not match size %d", end, size)
		}

		if _, err := body.Seek(0, io.SeekStart); err != nil {
			return fmt.Errorf("cachestore/s3 put: seek-rewind: %w", err)
		}
	}

	// Stat-then-put: if a peer already committed this chunk, skip the
	// (potentially multi-MiB) upload and report the existing object as
	// the winner.
	if _, err := d.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(d.bucket),
		Key:    aws.String(k.Path()),
	}); err == nil {
		d.log.LogAttrs(ctx, slog.LevelDebug, "cachestore_put_commit_lost",
			csChunkAttrs(k),
		)

		return cachestore.ErrCommitLost
	} else if !isNotFound(err) {
		// A non-404 Head error (auth, transient) is surfaced; only a
		// confirmed miss proceeds to the upload.
		mapped := mapErr(err)
		d.log.LogAttrs(ctx, slog.LevelDebug, "cachestore_put_head_err",
			csChunkAttrs(k),
			slog.Any("err", mapped),
		)

		return mapped
	}

	d.log.LogAttrs(ctx, slog.LevelDebug, "cachestore_put_chunk",
		csChunkAttrs(k),
		slog.Int64("size", size),
	)

	_, err := d.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(d.bucket),
		Key:           aws.String(k.Path()),
		Body:          body,
		ContentLength: aws.Int64(size),
	})
	if err != nil {
		mapped := mapErr(err)
		d.log.LogAttrs(ctx, slog.LevelDebug, "cachestore_put_err",
			csChunkAttrs(k),
			slog.Any("err", mapped),
		)

		return mapped
	}

	d.log.LogAttrs(ctx, slog.LevelDebug, "cachestore_put_success",
		csChunkAttrs(k),
		slog.Int64("size", size),
	)

	return nil
}

// Stat checks for chunk presence.
func (d *Driver) Stat(ctx context.Context, k chunk.Key) (cachestore.Info, error) {
	out, err := d.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(d.bucket),
		Key:    aws.String(k.Path()),
	})
	if err != nil {
		mapped := mapErr(err)
		// ErrNotFound is the expected 'miss' result for Stat; logged
		// at the same debug level as the hit path so cache-hit-rate
		// diagnostics can count both.
		d.log.LogAttrs(ctx, slog.LevelDebug, "cachestore_stat_result",
			csChunkAttrs(k),
			slog.Bool("present", false),
			slog.Any("err", mapped),
		)

		return cachestore.Info{}, mapped
	}

	info := cachestore.Info{}
	if out.ContentLength != nil {
		info.Size = *out.ContentLength
	}

	if out.LastModified != nil {
		// Recorded for the deferred active-eviction loop; no current
		// reader. See cachestore.Info.Committed.
		info.Committed = *out.LastModified
	}

	d.log.LogAttrs(ctx, slog.LevelDebug, "cachestore_stat_result",
		csChunkAttrs(k),
		slog.Bool("present", true),
		slog.Int64("size", info.Size),
	)

	return info, nil
}

// Delete removes the chunk; idempotent.
func (d *Driver) Delete(ctx context.Context, k chunk.Key) error {
	d.log.LogAttrs(ctx, slog.LevelDebug, "cachestore_delete",
		csChunkAttrs(k),
	)

	_, err := d.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(d.bucket),
		Key:    aws.String(k.Path()),
	})
	if err != nil {
		if isNotFound(err) {
			return nil
		}

		return mapErr(err)
	}

	return nil
}

// csChunkAttrs renders the chunk's identifying tuple as a slog
// group attribute matching the cross-package 'chunk' taxonomy used
// by fetch.Coordinator and chunkcatalog. Operator queries can grep
// on a single attribute path across the request lifecycle.
func csChunkAttrs(k chunk.Key) slog.Attr {
	return slog.Group("chunk",
		slog.String("origin_id", k.OriginID),
		slog.String("bucket", k.Bucket),
		slog.String("key", k.ObjectKey),
		slog.Int64("index", k.Index),
	)
}

func randHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failure is extraordinary on Linux. Surface it
		// to the selftest caller rather than masking with a
		// time-based fallback: a fallback could collide on parallel
		// boots and silently fail the first-put precondition, and
		// the underlying entropy / sandbox issue is operator-
		// actionable in its own right.
		return "", fmt.Errorf("cachestore/s3: rand.Read: %w", err)
	}

	return hex.EncodeToString(b), nil
}

func isNotFound(err error) bool {
	var nsk *s3types.NoSuchKey
	if errors.As(err, &nsk) {
		return true
	}

	var nsb *s3types.NoSuchBucket
	if errors.As(err, &nsb) {
		return true
	}

	var notFound *s3types.NotFound
	if errors.As(err, &notFound) {
		return true
	}

	var respErr *awshttp.ResponseError
	if errors.As(err, &respErr) && respErr.Response != nil &&
		respErr.Response.StatusCode == http.StatusNotFound {
		return true
	}

	return false
}

// mapErr normalises driver errors to the cachestore sentinel
// taxonomy. AccessDenied / Forbidden / Unauthorized are surfaced by
// the SDK with stable smithy.APIError codes so we keep that match
// path; everything else routes through HTTP status code on the
// underlying *awshttp.ResponseError.
func mapErr(err error) error {
	if isNotFound(err) {
		return cachestore.ErrNotFound
	}

	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "AccessDenied", "Unauthorized", "Forbidden", "InvalidAccessKeyId", "SignatureDoesNotMatch":
			return cachestore.ErrAuth
		}
	}

	var respErr *awshttp.ResponseError
	if errors.As(err, &respErr) && respErr.Response != nil {
		status := respErr.Response.StatusCode
		if status == http.StatusUnauthorized || status == http.StatusForbidden {
			return cachestore.ErrAuth
		}

		if status >= 500 && status < 600 {
			return cachestore.ErrTransient
		}
	}

	return err
}
