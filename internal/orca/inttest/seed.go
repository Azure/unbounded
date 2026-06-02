// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build integrationtest

package inttest

import (
	"bytes"
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// SeedBlob describes a single blob seeded into the origin.
type SeedBlob struct {
	Key  string
	Data []byte
}

// SmallBlob is one chunk's-worth (1 KiB).
func SmallBlob() SeedBlob {
	return SeedBlob{Key: "sample-1k", Data: deterministicBytes(1024, 0xa1)}
}

// MediumBlob spans two 1 MiB chunks.
func MediumBlob() SeedBlob {
	return SeedBlob{Key: "sample-2chunk", Data: deterministicBytes(1024*1024+512*1024, 0xb2)}
}

// HugeBlob spans 64 chunks at the harness's 1 MiB chunk size. With 3
// replicas, rendezvous-hashed coordinator selection statistically
// covers every replica many times over (~21 chunks per replica),
// so any test using HugeBlob exercises the full local-fill +
// cross-replica /internal/fill matrix in a single run.
func HugeBlob() SeedBlob {
	return SeedBlob{Key: "sample-64chunk", Data: deterministicBytes(64*1024*1024, 0xd4)}
}

// AllBlobs returns the canonical seed set used across most tests.
func AllBlobs() []SeedBlob {
	return []SeedBlob{SmallBlob(), MediumBlob(), HugeBlob()}
}

// SeedS3 uploads each blob to the named bucket via the provided
// Garage-friendly S3 client.
func SeedS3(ctx context.Context, t *testing.T, cli *s3.Client, bucket string, blobs []SeedBlob) {
	t.Helper()

	for _, b := range blobs {
		if _, err := cli.PutObject(ctx, &s3.PutObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(b.Key),
			Body:   bytes.NewReader(b.Data),
		}); err != nil {
			t.Fatalf("seed %s/%s: %v", bucket, b.Key, err)
		}
	}
}

// DeleteS3Object removes a blob from a Garage bucket. Used by
// warm-cache tests to prove that subsequent GETs are served from the
// cachestore and not refetched from the origin.
func DeleteS3Object(ctx context.Context, t *testing.T, cli *s3.Client, bucket, key string) {
	t.Helper()

	if _, err := cli.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	}); err != nil {
		t.Fatalf("delete origin %s/%s: %v", bucket, key, err)
	}
}

// SeedAzure uploads each blob to the named container as block blobs.
func SeedAzure(ctx context.Context, t *testing.T, az *Azurite, ctr string, blobs []SeedBlob) {
	t.Helper()

	for _, b := range blobs {
		az.UploadBlockBlob(ctx, t, ctr, b.Key, b.Data)
	}
}

// deterministicBytes returns n bytes filled with a repeating pattern
// derived from seed. Useful for byte-exact assertions without random
// flakiness.
func deterministicBytes(n int, seed byte) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = seed ^ byte(i*31+17)
	}

	return out
}
