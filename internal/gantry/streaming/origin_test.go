// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package streaming_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/httprange"
	"github.com/Azure/unbounded/internal/gantry/streaming"
)

func TestOriginClientFetchRange(t *testing.T) {
	t.Parallel()

	body := []byte("0123456789")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Range"); got != "bytes=2-5" {
			t.Errorf("Range = %q, want bytes=2-5", got)
		}

		if got := r.URL.RawQuery; got != "sig=a%2Bb%2Fc%3D&d=sha256:"+testDigest {
			t.Errorf("query = %q", got)
		}

		w.Header().Set("Content-Range", "bytes 2-5/10")
		w.Header().Set("Content-Length", "4")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(body[2:6]) //nolint:errcheck // best-effort write
	}))
	t.Cleanup(server.Close)

	raw := strings.Replace(server.URL, "127.0.0.1", "localhost", 1) + "?sig=a%2Bb%2Fc%3D&d=sha256:" + testDigest
	policy := streaming.URLPolicy{AllowedHostSuffixes: []string{"localhost"}, AllowHTTP: true}

	origin, err := streaming.ParseOriginURL(raw, policy)
	if err != nil {
		t.Fatal(err)
	}

	client, err := streaming.NewOriginClient(policy, 1, time.Second)
	if err != nil {
		t.Fatal(err)
	}

	rc, total, _, err := client.FetchRange(context.Background(), origin, httprange.Range{Start: 2, End: 5})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }() //nolint:errcheck // best-effort close

	if total != int64(len(body)) {
		t.Fatalf("total = %d, want %d", total, len(body))
	}

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}

	if string(got) != "2345" {
		t.Fatalf("body = %q, want 2345", got)
	}
}

func TestOriginClientRejectsInvalidRangeResponse(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Range", "bytes 3-6/10")
		w.Header().Set("Content-Length", "4")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte("3456")) //nolint:errcheck // best-effort write
	}))
	t.Cleanup(server.Close)

	raw := strings.Replace(server.URL, "127.0.0.1", "localhost", 1) + "?d=sha256:" + testDigest
	policy := streaming.URLPolicy{AllowedHostSuffixes: []string{"localhost"}, AllowHTTP: true}

	origin, err := streaming.ParseOriginURL(raw, policy)
	if err != nil {
		t.Fatal(err)
	}

	client, err := streaming.NewOriginClient(policy, 1, time.Second)
	if err != nil {
		t.Fatal(err)
	}

	rc, _, _, err := client.FetchRange(context.Background(), origin, httprange.Range{Start: 2, End: 5})
	if rc != nil {
		_ = rc.Close() //nolint:errcheck // best-effort close
	}

	if err == nil {
		t.Fatal("expected invalid range response error")
	}
}

func TestOriginClientRedirectRequiresSameDigest(t *testing.T) {
	t.Parallel()

	differentDigest := strings.Repeat("a", 64)
	final := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("redirect target should not be requested")
	}))
	t.Cleanup(final.Close)

	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		target := strings.Replace(final.URL, "127.0.0.1", "localhost", 1) + "?d=sha256:" + differentDigest
		http.Redirect(w, &http.Request{}, target, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(redirect.Close)

	policy := streaming.URLPolicy{AllowedHostSuffixes: []string{"localhost"}, AllowHTTP: true}
	raw := strings.Replace(redirect.URL, "127.0.0.1", "localhost", 1) + "?d=sha256:" + testDigest

	origin, err := streaming.ParseOriginURL(raw, policy)
	if err != nil {
		t.Fatal(err)
	}

	client, err := streaming.NewOriginClient(policy, 1, time.Second)
	if err != nil {
		t.Fatal(err)
	}

	_, _, _, err = client.FetchRange(context.Background(), origin, httprange.Range{Start: 0, End: 0})
	if err == nil || strings.Contains(err.Error(), "d=sha256") {
		t.Fatalf("error = %q, want redacted redirect failure", err)
	}
}

func TestOriginClientInvalidRequestErrorDoesNotExposeSignedQuery(t *testing.T) {
	t.Parallel()

	const secret = "secret-that-must-not-appear"

	d, err := digest.Parse("sha256:" + testDigest)
	if err != nil {
		t.Fatal(err)
	}

	client, err := streaming.NewOriginClient(
		streaming.URLPolicy{AllowedHostSuffixes: []string{"localhost"}, AllowHTTP: true},
		1,
		time.Second,
	)
	if err != nil {
		t.Fatal(err)
	}

	_, _, _, err = client.FetchRange(context.Background(), streaming.OriginURL{
		Raw:    "://invalid?sig=" + secret,
		Digest: d,
	}, httprange.Range{Start: 0, End: 0})
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("error = %q, want redacted request failure", err)
	}
}
