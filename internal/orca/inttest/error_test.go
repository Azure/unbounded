// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build integrationtest

package inttest

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// TestS3Error_NoSuchKey_XML verifies that GETting a non-existent
// object returns 404 with an S3-compatible <Error> envelope whose
// Code is "NoSuchKey". This is the on-wire end-to-end check that the
// XML reaches a real HTTP client correctly.
func TestS3Error_NoSuchKey_XML(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	bucket := pkgLocalStack.NewBucket(ctx, t, "orca-origin")

	cl := StartCluster(ctx, t, ClusterOptions{
		LocalStack:   pkgLocalStack,
		OriginBucket: bucket,
	})

	resp := cl.Get(1).HTTP.Get(ctx, t, bucket, "does-not-exist")
	if resp.Status != http.StatusNotFound {
		t.Fatalf("status=%d want 404; body=%s", resp.Status, string(resp.Body))
	}

	if got := resp.Header.Get("Content-Type"); got != "application/xml" {
		t.Errorf("Content-Type=%q want application/xml", got)
	}

	e := resp.ParseS3Error(t)
	if e.Code != "NoSuchKey" {
		t.Errorf("Code=%q want NoSuchKey", e.Code)
	}

	if e.Message == "" {
		t.Error("Message is empty")
	}
}

// TestS3Error_HeadNoSuchKey_NoBody verifies that a HEAD against a
// missing key returns 404 with no body (real S3 communicates HEAD
// failures via status code only). A non-empty body here would
// indicate the helper failed to suppress the body on HEAD.
func TestS3Error_HeadNoSuchKey_NoBody(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	bucket := pkgLocalStack.NewBucket(ctx, t, "orca-origin")

	cl := StartCluster(ctx, t, ClusterOptions{
		LocalStack:   pkgLocalStack,
		OriginBucket: bucket,
	})

	resp := cl.Get(1).HTTP.Head(ctx, t, bucket, "does-not-exist")
	if resp.Status != http.StatusNotFound {
		t.Fatalf("status=%d want 404", resp.Status)
	}

	if len(resp.Body) != 0 {
		t.Errorf("HEAD body must be empty; got %d bytes: %q", len(resp.Body), string(resp.Body))
	}
}

// TestS3Error_InvalidRange_XML verifies that an out-of-range Range
// header returns 416 with S3 InvalidRange code.
func TestS3Error_InvalidRange_XML(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	bucket := pkgLocalStack.NewBucket(ctx, t, "orca-origin")
	blob := SmallBlob()
	SeedS3(ctx, t, pkgLocalStack.NewS3Client(ctx, t), bucket, []SeedBlob{blob})

	cl := StartCluster(ctx, t, ClusterOptions{
		LocalStack:   pkgLocalStack,
		OriginBucket: bucket,
	})

	hdr := http.Header{}
	// Start position past EOF: unambiguously unsatisfiable per RFC.
	hdr.Set("Range", "bytes=99999999-")

	resp := cl.Get(1).HTTP.Do(ctx, t, http.MethodGet, "/"+bucket+"/"+blob.Key, hdr)
	if resp.Status != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("status=%d want 416; body=%s", resp.Status, string(resp.Body))
	}

	e := resp.ParseS3Error(t)
	if e.Code != "InvalidRange" {
		t.Errorf("Code=%q want InvalidRange", e.Code)
	}
}

// TestS3Error_ListObjectsV2_NotImplemented verifies that GET against
// a bucket root returns 501 with NotImplemented and a message naming
// ListObjectsV2.
func TestS3Error_ListObjectsV2_NotImplemented(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	bucket := pkgLocalStack.NewBucket(ctx, t, "orca-origin")

	cl := StartCluster(ctx, t, ClusterOptions{
		LocalStack:   pkgLocalStack,
		OriginBucket: bucket,
	})

	resp := cl.Get(1).HTTP.Do(ctx, t, http.MethodGet, "/"+bucket+"/", nil)
	if resp.Status != http.StatusNotImplemented {
		t.Fatalf("status=%d want 501; body=%s", resp.Status, string(resp.Body))
	}

	e := resp.ParseS3Error(t)
	if e.Code != "NotImplemented" {
		t.Errorf("Code=%q want NotImplemented", e.Code)
	}
}
