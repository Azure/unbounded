// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package netboot

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	pathpkg "path"
	"strings"
)

const maxTFTPArtifactBackendAttempts = 3

// HTTPArtifactBackend streams immutable session artifacts from a Metalman
// server and resumes a truncated backend response with exact byte ranges.
type HTTPArtifactBackend struct {
	backendURL *url.URL
	client     *http.Client
}

func NewHTTPArtifactBackend(backendURL string, client *http.Client) (*HTTPArtifactBackend, error) {
	backend, err := url.Parse(backendURL)
	if err != nil {
		return nil, fmt.Errorf("parsing artifact backend URL: %w", err)
	}

	if (backend.Scheme != "http" && backend.Scheme != "https") || backend.Host == "" {
		return nil, errors.New("artifact backend URL must use HTTP or HTTPS and include a host")
	}

	if client == nil {
		client = http.DefaultClient
	}

	return &HTTPArtifactBackend{backendURL: backend, client: client}, nil
}

func (b *HTTPArtifactBackend) Open(ctx context.Context, filename string) (io.ReadCloser, error) {
	requestURL := *b.backendURL
	requestURL.Path = pathpkg.Join(requestURL.Path, filename)

	reader := &resumingArtifactReader{ctx: ctx, client: b.client, url: requestURL.String()}
	if err := reader.open(0, -1); err != nil {
		return nil, err
	}

	return reader, nil
}

func (b *HTTPArtifactBackend) RecordBootLoaderDownloaded(ctx context.Context, filename string) error {
	parts := strings.Split(strings.TrimPrefix(filename, "/"), "/")
	if len(parts) < 7 || parts[5] != "artifacts" {
		return fmt.Errorf("invalid session artifact path %q", filename)
	}

	requestURL := *b.backendURL
	requestURL.Path = pathpkg.Join(requestURL.Path, pathpkg.Join(parts[:5]...), "callbacks", "boot-loader-downloaded")

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, requestURL.String(), nil)
	if err != nil {
		return fmt.Errorf("creating TFTP milestone request: %w", err)
	}

	response, err := b.client.Do(request)
	if err != nil {
		return fmt.Errorf("reporting TFTP milestone: %w", err)
	}
	defer response.Body.Close() //nolint:errcheck // Response body is not reused.

	if response.StatusCode != http.StatusNoContent {
		return fmt.Errorf("reporting TFTP milestone: backend returned %s", response.Status)
	}

	return nil
}

type resumingArtifactReader struct {
	ctx      context.Context
	client   *http.Client
	url      string
	body     io.ReadCloser
	offset   int64
	end      int64
	attempts int
}

func (r *resumingArtifactReader) Read(buffer []byte) (int, error) {
	for {
		n, err := r.body.Read(buffer)
		r.offset += int64(n)

		truncated := errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
		if !truncated || r.offset > r.end {
			return n, err
		}

		if n > 0 {
			return n, nil
		}

		if r.attempts >= maxTFTPArtifactBackendAttempts {
			return 0, io.ErrUnexpectedEOF
		}

		if err := r.open(r.offset, r.end); err != nil {
			if r.attempts >= maxTFTPArtifactBackendAttempts {
				return 0, err
			}
		}
	}
}

func (r *resumingArtifactReader) Close() error {
	if r.body == nil {
		return nil
	}

	return r.body.Close()
}

func (r *resumingArtifactReader) open(start, end int64) error {
	r.attempts++

	request, err := http.NewRequestWithContext(r.ctx, http.MethodGet, r.url, nil)
	if err != nil {
		return err
	}

	if end >= 0 {
		request.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
	}

	response, err := r.client.Do(request)
	if err != nil {
		return fmt.Errorf("requesting TFTP artifact backend: %w", err)
	}

	wantStatus := http.StatusOK
	if end >= 0 {
		wantStatus = http.StatusPartialContent
	}

	if response.StatusCode != wantStatus || response.ContentLength <= 0 {
		response.Body.Close() //nolint:errcheck // Invalid response.
		return fmt.Errorf("TFTP artifact backend returned %s", response.Status)
	}

	if end < 0 {
		end = response.ContentLength - 1
	} else if response.ContentLength != end-start+1 || response.Header.Get("Content-Range") != fmt.Sprintf("bytes %d-%d/%d", start, end, end+1) {
		response.Body.Close() //nolint:errcheck // Invalid range response.
		return errors.New("TFTP artifact backend returned a mismatched range")
	}

	if r.body != nil {
		r.body.Close() //nolint:errcheck // Previous response reached EOF.
	}

	r.body = response.Body
	r.end = end

	return nil
}
