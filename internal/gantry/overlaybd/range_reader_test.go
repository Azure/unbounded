// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package overlaybd

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRangeReaderReadsExactGantryRange(t *testing.T) {
	data := []byte("0123456789")

	var gotPath, gotRange string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotRange = r.Header.Get("Range")
		w.Header().Set("Content-Range", "bytes 2-5/10")
		w.Header().Set("Content-Length", "4")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(data[2:6])
	}))
	t.Cleanup(server.Close)

	reader, err := NewRangeReader(server.Client(), server.URL+"/blobs", "https://acr.azurecr.io/v2/team/app/blobs/"+digestB, int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}

	buffer := make([]byte, 4)

	n, err := reader.ReadAt(buffer, 2)
	if err != nil || n != 4 || string(buffer) != "2345" {
		t.Fatalf("ReadAt = %d, %q, %v", n, buffer, err)
	}

	if gotRange != "bytes=2-5" || gotPath != "/blobs/https://acr.azurecr.io/v2/team/app/blobs/"+digestB {
		t.Fatalf("request = path %q range %q", gotPath, gotRange)
	}
}

func TestRangeReaderReturnsEOFAtObjectEnd(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Range", "bytes 8-9/10")
		w.Header().Set("Content-Length", "2")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, "89")
	}))
	t.Cleanup(server.Close)

	reader, err := NewRangeReader(server.Client(), server.URL+"/blobs", "https://acr.azurecr.io/v2/team/app/blobs/"+digestB, 10)
	if err != nil {
		t.Fatal(err)
	}

	buffer := make([]byte, 4)

	n, err := reader.ReadAt(buffer, 8)
	if n != 2 || !errors.Is(err, io.EOF) || string(buffer[:n]) != "89" {
		t.Fatalf("ReadAt = %d, %q, %v", n, buffer[:n], err)
	}
}

func TestRangeReaderRejectsInvalidResponsesAndEndpoints(t *testing.T) {
	for _, endpoint := range []string{"https://127.0.0.1/blobs", "http://example.com/blobs"} {
		if _, err := NewRangeReader(nil, endpoint, "https://acr.azurecr.io/v2/repo/blobs/"+digestB, 10); err == nil {
			t.Fatalf("endpoint %q accepted", endpoint)
		}
	}

	for _, test := range []struct {
		name         string
		status       int
		contentRange string
		body         string
		want         string
	}{
		{name: "status", status: http.StatusOK, contentRange: "bytes 0-3/10", body: "0123", want: "status 200"},
		{name: "range", status: http.StatusPartialContent, contentRange: "bytes 1-4/10", body: "0123", want: "Content-Range"},
		{name: "short", status: http.StatusPartialContent, contentRange: "bytes 0-3/10", body: "01", want: "Content-Length"},
		{name: "long", status: http.StatusPartialContent, contentRange: "bytes 0-3/10", body: "01234", want: "Content-Length"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Range", test.contentRange)
				w.Header().Set("Content-Length", fmt.Sprint(len(test.body)))
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, test.body)
			}))
			t.Cleanup(server.Close)

			reader, err := NewRangeReader(server.Client(), server.URL+"/blobs", "https://acr.azurecr.io/v2/repo/blobs/"+digestB, 10)
			if err != nil {
				t.Fatal(err)
			}

			_, err = reader.ReadAt(make([]byte, 4), 0)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}
