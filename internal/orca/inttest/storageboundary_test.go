// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//go:build integrationtest && storageboundary

package inttest

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// storageboundary_test.go is the unbounded-storage -> orca -> Garage
// boundary test. It verifies the unbounded-storage S3 frontend
// implementation end to end: a client GET against an unbounded-storage
// frontend flows through the storage stack, out the unbounded-storage
// S3 backend (unsigned, plaintext, path-style HTTP/1.1), into an
// in-process orca edge listener (orca's S3 surface), through orca's
// awss3 origin driver, and finally to the same Garage S3 backend orca's
// own integration tests run against. Orca additionally chunk-caches to
// a Garage cachestore bucket, so the data path exercised here is the
// full production read path.
//
// It runs the storage binaries out of process (a two-node libfabric tcp
// ring) and so requires `make unbounded-storage-build` plus sudo for
// RLIMIT_MEMLOCK (io_uring pinned buffers). The dedicated build tag
// keeps it out of `make orca-inttest`; run it with
// `make orca-inttest-storage`.

// storageMultipartThreshold is the object size above which seeding uses
// a streaming multipart upload instead of a single buffered PutObject,
// so the 1 GiB blob is never materialized whole in memory.
const storageMultipartThreshold = 64 * 1024 * 1024

// storageMultipartPartSize is the per-part size for streaming uploads.
// It is well above S3's 5 MiB minimum and divides 1 GiB evenly.
const storageMultipartPartSize = 64 * 1024 * 1024

// boundaryKey describes one object exercised by the boundary test.
type boundaryKey struct {
	name string
	key  string
	size int64
	seed byte
}

// boundaryKeys is the key-size matrix required by the test: an empty
// object, one smaller than a single storage page, one slightly larger
// than a single page, and one 1 GiB object that spans many stripes.
func boundaryKeys() []boundaryKey {
	return []boundaryKey{
		{name: "empty", key: "boundary-empty", size: 0, seed: 0x10},
		{name: "sub-page", key: "boundary-sub-page", size: storagePageSize / 2, seed: 0x20},
		{name: "over-page", key: "boundary-over-page", size: storagePageSize + 512, seed: 0x30},
		{name: "huge-1gib", key: "boundary-huge-1gib", size: 1 << 30, seed: 0x40},
	}
}

// TestStorageBoundaryThroughOrca seeds the matrix into a Garage origin
// bucket, points a single-replica orca cluster at it, then fetches each
// key through both unbounded-storage frontends (the non-owning frontend
// forces the cross-node fabric RPC) and stream-verifies the bytes.
func TestStorageBoundaryThroughOrca(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	bucket := pkgS3.NewBucket(ctx, t, "orca-origin")
	cli := pkgS3.NewS3Client(ctx, t)

	keys := boundaryKeys()
	for _, k := range keys {
		seedBoundaryObject(ctx, t, cli, bucket, k)
	}

	// Single orca replica is sufficient: the cross-node coverage we
	// care about here is in the unbounded-storage ring, not orca's
	// cluster. Bump the chunk size to 8 MiB to keep the 1 GiB object's
	// chunk count (and cachestore round-trips) manageable.
	cl := StartCluster(ctx, t, ClusterOptions{
		Replicas:     1,
		S3Backend:    pkgS3,
		OriginBucket: bucket,
		ChunkSize:    8 * 1024 * 1024,
	})

	edge := cl.Get(1).App.EdgeAddr

	ring := startStorageRing(ctx, t, edge)

	for _, k := range keys {
		t.Run(k.name, func(t *testing.T) {
			for i, fe := range ring.FrontendAddrs {
				label := fmt.Sprintf("%s via frontend %d", k.name, i)
				fetchAndVerify(ctx, t, label, fe, bucket, k)
			}
		})
	}
}

// fetchAndVerify GETs the key through one unbounded-storage frontend and
// stream-verifies the body against the deterministic pattern, never
// buffering the whole object.
func fetchAndVerify(ctx context.Context, t *testing.T, label, frontendAddr, bucket string, k boundaryKey) {
	t.Helper()

	url := httpObjectURL(frontendAddr, bucket, k.key)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("%s: build request: %v", label, err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s: GET %s: %v", label, url, err)
	}

	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s: status=%d (want 200)", label, resp.StatusCode)
	}

	verifyDeterministicStream(t, label, resp.Body, k.size, k.seed)
}

// seedBoundaryObject uploads one matrix object to the origin bucket,
// streaming multipart for the large blob and a single buffered
// PutObject for the small ones.
func seedBoundaryObject(ctx context.Context, t *testing.T, cli *s3.Client, bucket string, k boundaryKey) {
	t.Helper()

	if k.size >= storageMultipartThreshold {
		seedLargeObject(ctx, t, cli, bucket, k.key, k.size, k.seed)
		return
	}

	data := make([]byte, k.size)
	fillDeterministic(data, 0, k.seed)

	if _, err := cli.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(k.key),
		Body:   bytes.NewReader(data),
	}); err != nil {
		t.Fatalf("seed %s/%s: %v", bucket, k.key, err)
	}
}

// seedLargeObject performs a streaming multipart upload of `size`
// deterministic bytes, reusing a single part-sized buffer so a 1 GiB
// object is never held whole in memory.
func seedLargeObject(ctx context.Context, t *testing.T, cli *s3.Client, bucket, key string, size int64, seed byte) {
	t.Helper()

	create, err := cli.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("create multipart %s/%s: %v", bucket, key, err)
	}

	uploadID := create.UploadId

	var (
		parts   []s3types.CompletedPart
		partNum int32 = 1
		buf           = make([]byte, storageMultipartPartSize)
	)

	for off := int64(0); off < size; off += storageMultipartPartSize {
		n := int64(storageMultipartPartSize)
		if remaining := size - off; n > remaining {
			n = remaining
		}

		fillDeterministic(buf[:n], off, seed)

		up, err := cli.UploadPart(ctx, &s3.UploadPartInput{
			Bucket:     aws.String(bucket),
			Key:        aws.String(key),
			UploadId:   uploadID,
			PartNumber: aws.Int32(partNum),
			Body:       bytes.NewReader(buf[:n]),
		})
		if err != nil {
			_, _ = cli.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{ //nolint:errcheck // best-effort
				Bucket: aws.String(bucket), Key: aws.String(key), UploadId: uploadID,
			})
			t.Fatalf("upload part %d of %s/%s: %v", partNum, bucket, key, err)
		}

		parts = append(parts, s3types.CompletedPart{
			ETag:       up.ETag,
			PartNumber: aws.Int32(partNum),
		})
		partNum++
	}

	if _, err := cli.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:          aws.String(bucket),
		Key:             aws.String(key),
		UploadId:        uploadID,
		MultipartUpload: &s3types.CompletedMultipartUpload{Parts: parts},
	}); err != nil {
		t.Fatalf("complete multipart %s/%s: %v", bucket, key, err)
	}
}
