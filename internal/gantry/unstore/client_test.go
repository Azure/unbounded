// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package unstore_test

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/unstore"
)

func ref(repo, dgst string, kind ifaces.OriginRefKind) ifaces.OriginRef {
	return ifaces.OriginRef{
		Registry:   "registry.example.com",
		Repository: repo,
		Digest:     digest.MustParse(dgst),
		Kind:       kind,
	}
}

func TestPull_Hit(t *testing.T) {
	body := "hello blob"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/library/nginx/blobs/sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}

		w.Header().Set("Content-Length", "10")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	c := unstore.New(srv.URL, 0)
	rc, size, err := c.Pull(context.Background(), ref("library/nginx", "sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08", ifaces.KindBlob))
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}

	defer rc.Close()

	if size != 10 {
		t.Errorf("size = %d, want 10", size)
	}

	got, _ := io.ReadAll(rc)
	if string(got) != body {
		t.Errorf("body = %q, want %q", got, body)
	}
}

func TestPull_ManifestPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/library/nginx/manifests/sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}

		w.Header().Set("Content-Length", "5")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("hello"))
	}))
	defer srv.Close()

	c := unstore.New(srv.URL, 0)
	rc, _, err := c.Pull(context.Background(), ref("library/nginx", "sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08", ifaces.KindManifest))
	if err != nil {
		t.Fatalf("Pull manifest: %v", err)
	}

	_ = rc.Close()
}

func TestPull_MissEOF(t *testing.T) {
	// Simulate unbounded-storage miss: close connection before sending any response.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	go func() {
		conn, _ := ln.Accept()
		if conn != nil {
			conn.Close()
		}
	}()

	endpoint := "http://" + ln.Addr().String()
	c := unstore.New(endpoint, 5*time.Second)
	_, _, pullErr := c.Pull(context.Background(), ref("library/nginx", "sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08", ifaces.KindBlob))

	var oe *ifaces.OriginError
	if !errors.As(pullErr, &oe) {
		t.Fatalf("want *OriginError, got %T: %v", pullErr, pullErr)
	}

	if oe.Class != ifaces.FailureNotFound {
		t.Errorf("class = %q, want FailureNotFound", oe.Class)
	}

	_ = ln.Close()
}

func TestPull_ConnectionRefused(t *testing.T) {
	// Use a port that is not listening.
	c := unstore.New("http://127.0.0.1:1", 2*time.Second)
	_, _, err := c.Pull(context.Background(), ref("library/nginx", "sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08", ifaces.KindBlob))

	var oe *ifaces.OriginError
	if !errors.As(err, &oe) {
		t.Fatalf("want *OriginError, got %T: %v", err, err)
	}

	if oe.Class != ifaces.FailureTransient {
		t.Errorf("class = %q, want FailureTransient", oe.Class)
	}
}

func TestPull_Non200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	c := unstore.New(srv.URL, 0)
	_, _, err := c.Pull(context.Background(), ref("library/nginx", "sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08", ifaces.KindBlob))

	var oe *ifaces.OriginError
	if !errors.As(err, &oe) {
		t.Fatalf("want *OriginError, got %T: %v", err, err)
	}

	if oe.Class != ifaces.FailureTransient {
		t.Errorf("class = %q, want FailureTransient", oe.Class)
	}
}

func TestPull_Timeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Never respond - simulate a hung server.
		<-r.Context().Done()
	}))
	defer srv.Close()

	c := unstore.New(srv.URL, 100*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, _, err := c.Pull(ctx, ref("library/nginx", "sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08", ifaces.KindBlob))

	var oe *ifaces.OriginError
	if !errors.As(err, &oe) {
		t.Fatalf("want *OriginError, got %T: %v", err, err)
	}

	if oe.Class != ifaces.FailureTransient {
		t.Errorf("class = %q, want FailureTransient", oe.Class)
	}
}

func TestHead_Hit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			t.Errorf("method = %s, want HEAD", r.Method)
		}

		w.Header().Set("Content-Length", "42")
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := unstore.New(srv.URL, 0)
	size, ct, err := c.Head(context.Background(), ref("library/nginx", "sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08", ifaces.KindBlob))
	if err != nil {
		t.Fatalf("Head: %v", err)
	}

	if size != 42 {
		t.Errorf("size = %d, want 42", size)
	}

	if ct != "application/octet-stream" {
		t.Errorf("content-type = %q, want application/octet-stream", ct)
	}
}

func TestHead_MissEOF(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	go func() {
		conn, _ := ln.Accept()
		if conn != nil {
			conn.Close()
		}
	}()

	c := unstore.New("http://"+ln.Addr().String(), 5*time.Second)
	_, _, headErr := c.Head(context.Background(), ref("library/nginx", "sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08", ifaces.KindBlob))

	var oe *ifaces.OriginError
	if !errors.As(headErr, &oe) {
		t.Fatalf("want *OriginError, got %T: %v", headErr, headErr)
	}

	if oe.Class != ifaces.FailureNotFound {
		t.Errorf("class = %q, want FailureNotFound", oe.Class)
	}

	_ = ln.Close()
}

func TestHead_ConnectionRefused(t *testing.T) {
	c := unstore.New("http://127.0.0.1:1", 2*time.Second)
	_, _, err := c.Head(context.Background(), ref("library/nginx", "sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08", ifaces.KindBlob))

	var oe *ifaces.OriginError
	if !errors.As(err, &oe) {
		t.Fatalf("want *OriginError, got %T: %v", err, err)
	}

	if oe.Class != ifaces.FailureTransient {
		t.Errorf("class = %q, want FailureTransient", oe.Class)
	}
}

// Ensure Client implements ifaces.OriginPuller at compile time.
var _ ifaces.OriginPuller = (*unstore.Client)(nil)
