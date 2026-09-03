// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// parseS3Error parses the body of a httptest.ResponseRecorder as an
// S3 <Error> envelope. Test helper; fails the test on parse error.
func parseS3Error(t *testing.T, rr *httptest.ResponseRecorder) s3ErrorBody {
	t.Helper()

	body := rr.Body.Bytes()
	if !strings.HasPrefix(string(body), xml.Header) {
		t.Errorf("body missing XML declaration; got %q", string(body))
	}

	var e s3ErrorBody
	if err := xml.Unmarshal(body, &e); err != nil {
		t.Fatalf("unmarshal s3 error: %v; body=%q", err, string(body))
	}

	return e
}

func TestWriteS3Error_GET(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodGet, "/bucket/key", nil)
	rr := httptest.NewRecorder()

	writeS3Error(rr, req, http.StatusNotFound, s3ErrNoSuchKey,
		"The specified key does not exist.",
		withBucketKey("bucket", "key"))

	if rr.Code != http.StatusNotFound {
		t.Errorf("status=%d want %d", rr.Code, http.StatusNotFound)
	}

	if got := rr.Header().Get("Content-Type"); got != "application/xml" {
		t.Errorf("Content-Type=%q want application/xml", got)
	}

	if got := rr.Header().Get("Server"); got != "orca" {
		t.Errorf("Server=%q want orca", got)
	}

	body := parseS3Error(t, rr)
	if body.Code != s3ErrNoSuchKey {
		t.Errorf("Code=%q want %q", body.Code, s3ErrNoSuchKey)
	}

	if body.Message == "" {
		t.Error("Message is empty")
	}

	if body.BucketName != "bucket" || body.Key != "key" {
		t.Errorf("bucket/key=%q/%q want bucket/key", body.BucketName, body.Key)
	}

	if body.Resource != "/bucket/key" {
		t.Errorf("Resource=%q want /bucket/key", body.Resource)
	}
}

func TestWriteS3Error_HEAD_NoBody(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodHead, "/bucket/key", nil)
	rr := httptest.NewRecorder()

	writeS3Error(rr, req, http.StatusNotFound, s3ErrNoSuchKey,
		"The specified key does not exist.")

	if rr.Code != http.StatusNotFound {
		t.Errorf("status=%d want %d", rr.Code, http.StatusNotFound)
	}

	if got := rr.Header().Get("Server"); got != "orca" {
		t.Errorf("Server=%q want orca", got)
	}

	if rr.Body.Len() != 0 {
		t.Errorf("HEAD body must be empty; got %d bytes: %q", rr.Body.Len(), rr.Body.String())
	}

	// HEAD must not advertise an XML content-type since there is no
	// body. (A non-empty Content-Type with a zero-length body would
	// confuse some SDKs into trying to parse.)
	if got := rr.Header().Get("Content-Type"); got != "" {
		t.Errorf("Content-Type=%q want empty on HEAD", got)
	}
}

func TestWriteS3Error_OmitsEmptyOptionalFields(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()

	writeS3Error(rr, req, http.StatusNotImplemented, s3ErrNotImplemented,
		"ListBuckets is not implemented by orca.")

	body := rr.Body.String()
	for _, tag := range []string{"<BucketName>", "<Key>"} {
		if strings.Contains(body, tag) {
			t.Errorf("body should omit %s when empty; got %q", tag, body)
		}
	}
}

func TestWriteS3Error_NilRequest(t *testing.T) {
	t.Parallel()

	// Defensive: helper must tolerate a nil request (e.g. from
	// internal tests that don't construct one). The body should
	// still be written and HEAD-suppression should default to false.
	rr := httptest.NewRecorder()
	writeS3Error(rr, nil, http.StatusInternalServerError, "InternalError", "boom")

	if rr.Code != http.StatusInternalServerError {
		t.Errorf("status=%d", rr.Code)
	}

	body := parseS3Error(t, rr)
	if body.Code != "InternalError" || body.Message != "boom" {
		t.Errorf("body=%+v", body)
	}
}
