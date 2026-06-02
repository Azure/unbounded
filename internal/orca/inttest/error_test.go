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

// TestS3Errors exercises orca's edge error surface on the wire (raw
// net/http requests). It verifies that the S3-compatible <Error> XML
// envelope, status codes, and HEAD-no-body behavior reach a real
// HTTP client correctly. Companion suite TestS3SDK (s3sdk_test.go)
// re-checks the same surface through an actual S3 SDK so we know
// SDKs unmarshal the envelope into typed errors.
//
// A single cluster is shared across all subtests; subtests are
// independent and run in parallel.
func TestS3Errors(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
	// Parallel subtests are paused until the parent returns, then
	// resumed; a `defer cancel()` would cancel the context before
	// they ever run. Use t.Cleanup so the cancel fires after all
	// subtests finish instead.
	t.Cleanup(cancel)

	bucket := pkgGarage.NewBucket(ctx, t, "orca-origin")
	blob := SmallBlob()
	SeedS3(ctx, t, pkgGarage.NewS3Client(ctx, t), bucket, []SeedBlob{blob})

	cl := StartCluster(ctx, t, ClusterOptions{
		Garage:       pkgGarage,
		OriginBucket: bucket,
	})

	httpClient := cl.Get(1).HTTP

	// NoSuchKey_XML: GET against a missing object returns 404 with
	// the standard S3 <Error> envelope and Code "NoSuchKey".
	t.Run("NoSuchKey_XML", func(t *testing.T) {
		t.Parallel()

		resp := httpClient.Get(ctx, t, bucket, "does-not-exist")
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
	})

	// HeadNoSuchKey_NoBody: HEAD against a missing object returns
	// 404 with an empty body, mirroring real S3 (HEAD must not have
	// a body per RFC 9110; SDKs key off the status code).
	t.Run("HeadNoSuchKey_NoBody", func(t *testing.T) {
		t.Parallel()

		resp := httpClient.Head(ctx, t, bucket, "does-not-exist")
		if resp.Status != http.StatusNotFound {
			t.Fatalf("status=%d want 404", resp.Status)
		}

		if len(resp.Body) != 0 {
			t.Errorf("HEAD body must be empty; got %d bytes: %q", len(resp.Body), string(resp.Body))
		}
	})

	// InvalidRange_XML: an unsatisfiable Range returns 416 with
	// Code "InvalidRange".
	t.Run("InvalidRange_XML", func(t *testing.T) {
		t.Parallel()

		hdr := http.Header{}
		// Start position past EOF: unambiguously unsatisfiable per RFC.
		hdr.Set("Range", "bytes=99999999-")

		resp := httpClient.Do(ctx, t, http.MethodGet, "/"+bucket+"/"+blob.Key, hdr)
		if resp.Status != http.StatusRequestedRangeNotSatisfiable {
			t.Fatalf("status=%d want 416; body=%s", resp.Status, string(resp.Body))
		}

		e := resp.ParseS3Error(t)
		if e.Code != "InvalidRange" {
			t.Errorf("Code=%q want InvalidRange", e.Code)
		}
	})

	// ListObjectsV2_NotImplemented: GET against a bucket root
	// returns 501 with Code "NotImplemented" and a message naming
	// ListObjectsV2 (per the routing split in EdgeHandler.ServeHTTP).
	t.Run("ListObjectsV2_NotImplemented", func(t *testing.T) {
		t.Parallel()

		resp := httpClient.Do(ctx, t, http.MethodGet, "/"+bucket+"/", nil)
		if resp.Status != http.StatusNotImplemented {
			t.Fatalf("status=%d want 501; body=%s", resp.Status, string(resp.Body))
		}

		e := resp.ParseS3Error(t)
		if e.Code != "NotImplemented" {
			t.Errorf("Code=%q want NotImplemented", e.Code)
		}
	})
}
