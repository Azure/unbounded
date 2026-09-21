// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package fixture implements the origin wire contract and in-cluster HTTP probes.
package fixture

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const ObjectSize = 16384

func ETag(version int) string { return fmt.Sprintf(`"%x"`, sha256.Sum256(Body(version))) }

func Body(version int) []byte {
	b := make([]byte, ObjectSize)
	for i := range b {
		b[i] = byte((i*31 + version*17) % 251)
	}

	return b
}

type Response struct {
	Status int
	Header http.Header
	Body   []byte
}

func Fetch(method, url, byteRange string, headers ...string) (Response, error) {
	r, err := http.NewRequest(method, url, nil)
	if err != nil {
		return Response{}, err
	}

	if byteRange != "" {
		r.Header.Set("Range", byteRange)
	}

	for _, header := range headers {
		key, value, ok := strings.Cut(header, ":")
		if !ok {
			return Response{}, fmt.Errorf("invalid header %q", header)
		}

		r.Header.Set(key, strings.TrimSpace(value))
	}

	c := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DisableCompression: true}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer c.CloseIdleConnections()

	resp, err := c.Do(r)
	if err != nil {
		return Response{}, err
	}

	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))

	return Response{resp.StatusCode, resp.Header, b}, errors.Join(err, resp.Body.Close())
}

type Hit struct {
	Method  string
	Target  string
	Source  string
	Version int
}

type Origin struct {
	mu      sync.Mutex
	version int
	hits    []Hit
}

func NewOrigin() *Origin { return &Origin{version: 1} }

func (o *Origin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	o.mu.Lock()
	defer o.mu.Unlock()

	switch r.URL.EscapedPath() {
	case "/healthz":
		w.WriteHeader(http.StatusOK)
		return
	case "/hits":
		if err := json.NewEncoder(w).Encode(o.hits); err != nil {
			log.Printf("write hit ledger: %v", err)
		}

		return
	case "/version":
		v, err := strconv.Atoi(r.URL.Query().Get("value"))
		if r.Method != http.MethodPost || err != nil || v < 1 {
			http.Error(w, "invalid version", 400)
			return
		}

		o.version = v

		return
	}

	target := r.RequestURI
	if target == "" || (r.Method != "HEAD" && r.Method != "GET") {
		http.Error(w, "invalid backend request", 400)
		return
	}

	source, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		http.Error(w, "invalid remote address", http.StatusBadRequest)
		return
	}

	o.hits = append(o.hits, Hit{r.Method, target, source, o.version})
	if strings.HasPrefix(target, "/missing") {
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusNotFound)

		return
	}

	body := Body(o.version)
	etag := ETag(o.version)
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "max-age=3600")
	w.Header().Set("Accept-Ranges", "bytes")

	if r.Method == "HEAD" {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		return
	}

	if r.Header.Get("If-Match") != etag {
		w.WriteHeader(http.StatusPreconditionFailed)
		return
	}

	var start, end int

	value := r.Header.Get("Range")
	if n, err := fmt.Sscanf(value, "bytes=%d-%d", &start, &end); err != nil || n != 2 || start < 0 || end < start || end >= len(body) || value != fmt.Sprintf("bytes=%d-%d", start, end) {
		http.Error(w, "invalid page range", http.StatusRequestedRangeNotSatisfiable)
		return
	}

	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(body)))
	w.Header().Set("Content-Length", strconv.Itoa(end-start+1))
	w.WriteHeader(http.StatusPartialContent)

	if _, err := w.Write(body[start : end+1]); err != nil {
		log.Printf("write page: %v", err)
	}
}
