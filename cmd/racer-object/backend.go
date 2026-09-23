// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"

	racer "github.com/Azure/unbounded/pkg/racer"
)

type blobMetadata struct {
	size int64
	etag azcore.ETag
}

// blobSource isolates cloud metadata and range operations for future providers.
type blobSource interface {
	stat(context.Context, objectSpec) (blobMetadata, error)
	read(context.Context, objectSpec, blobMetadata, []byte, int64) error
}

type azureSource struct{ client *azblob.Client }

func azureClient(endpoint, auth string, concurrency int) (*azureSource, error) {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, fmt.Errorf("default HTTP transport has unexpected type")
	}

	transport := base.Clone()
	transport.MaxIdleConns = concurrency
	transport.MaxIdleConnsPerHost = concurrency
	transport.MaxConnsPerHost = concurrency
	transport.DisableCompression = true
	options := &azblob.ClientOptions{ClientOptions: azcore.ClientOptions{
		Transport: &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		Retry:     policy.RetryOptions{MaxRetries: 3, TryTimeout: 2 * time.Minute},
	}}

	var (
		client *azblob.Client
		err    error
	)

	switch auth {
	case "workload-identity":
		var credential *azidentity.WorkloadIdentityCredential

		credential, err = azidentity.NewWorkloadIdentityCredential(nil)
		if err == nil {
			client, err = azblob.NewClient(endpoint, credential, options)
		}
	case "default":
		var credential *azidentity.DefaultAzureCredential

		credential, err = azidentity.NewDefaultAzureCredential(nil)
		if err == nil {
			client, err = azblob.NewClient(endpoint, credential, options)
		}
	case "shared-key":
		var credential *azblob.SharedKeyCredential

		credential, err = azblob.NewSharedKeyCredential(os.Getenv("AZURE_STORAGE_ACCOUNT"), os.Getenv("AZURE_STORAGE_KEY"))
		if err == nil {
			client, err = azblob.NewClientWithSharedKeyCredential(endpoint, credential, options)
		}
	case "anonymous":
		client, err = azblob.NewClientWithNoCredential(endpoint, options)
	default:
		return nil, fmt.Errorf("unknown Azure auth mode %q", auth)
	}

	if err != nil {
		return nil, err
	}

	return &azureSource{client}, nil
}

func (a *azureSource) stat(ctx context.Context, o objectSpec) (blobMetadata, error) {
	r, err := a.client.ServiceClient().NewContainerClient(o.Container).NewBlobClient(o.Blob).GetProperties(ctx, nil)
	if err != nil {
		return blobMetadata{}, cloudError(err)
	}

	if r.ContentLength == nil || *r.ContentLength < 0 || r.ETag == nil || *r.ETag == "" {
		return blobMetadata{}, fmt.Errorf("azure returned incomplete metadata")
	}

	return blobMetadata{*r.ContentLength, *r.ETag}, nil
}

func (a *azureSource) read(ctx context.Context, o objectSpec, m blobMetadata, p []byte, off int64) error {
	r, err := a.client.DownloadStream(ctx, o.Container, o.Blob, &azblob.DownloadStreamOptions{
		Range:            blob.HTTPRange{Offset: off, Count: int64(len(p))},
		AccessConditions: &blob.AccessConditions{ModifiedAccessConditions: &blob.ModifiedAccessConditions{IfMatch: &m.etag}},
	})
	if err != nil {
		return cloudError(err)
	}

	if r.ContentLength == nil || *r.ContentLength != int64(len(p)) || r.ETag == nil || *r.ETag != m.etag ||
		r.ContentRange == nil || *r.ContentRange != fmt.Sprintf("bytes %d-%d/%d", off, off+int64(len(p))-1, m.size) {
		closeResource(r.Body)
		return fmt.Errorf("azure range response does not match requested snapshot")
	}
	// The SDK retry reader resumes interrupted bodies with the response ETag.
	body := r.NewRetryReader(ctx, &blob.RetryReaderOptions{MaxRetries: 3})
	defer closeResource(body)

	_, err = io.ReadFull(body, p)

	return err
}

func cloudError(err error) error {
	var response *azcore.ResponseError
	if errors.As(err, &response) {
		switch response.StatusCode {
		case 404:
			return fs.ErrNotExist
		case 401, 403:
			return fs.ErrPermission
		case 412:
			return racer.ErrVersionChanged
		}
	}

	return err
}

type metadataEntry struct {
	object  objectSpec
	mu      sync.Mutex
	meta    *blobMetadata
	loading chan struct{}
}

type backend struct {
	source  blobSource
	entries map[string]*metadataEntry
	slots   chan struct{}
	buffers chan []byte
	ttl     time.Duration
}

func newBackend(c *configuration, source blobSource, concurrency int) *backend {
	b := &backend{source: source, entries: make(map[string]*metadataEntry), slots: make(chan struct{}, concurrency), buffers: make(chan []byte, concurrency), ttl: 24 * time.Hour}
	for _, o := range c.Objects {
		b.entries[o.target] = &metadataEntry{object: o}
	}

	return b
}

func (b *backend) metadata(ctx context.Context, target string) (*metadataEntry, blobMetadata, error) {
	e, ok := b.entries[target]
	if !ok {
		return nil, blobMetadata{}, fs.ErrNotExist
	}

	for {
		if err := ctx.Err(); err != nil {
			return nil, blobMetadata{}, err
		}

		e.mu.Lock()
		if e.meta != nil {
			m := *e.meta
			e.mu.Unlock()

			return e, m, nil
		}

		if e.loading != nil {
			done := e.loading
			e.mu.Unlock()

			select {
			case <-done:
				continue
			case <-ctx.Done():
				return nil, blobMetadata{}, ctx.Err()
			}
		}

		e.loading = make(chan struct{})
		e.mu.Unlock()

		var (
			m   blobMetadata
			err error
		)

		select {
		case b.slots <- struct{}{}:
			m, err = b.source.stat(ctx, e.object)
			<-b.slots
		case <-ctx.Done():
			err = ctx.Err()
		}

		e.mu.Lock()
		if err == nil {
			e.meta = &m
		}

		close(e.loading)
		e.loading = nil
		e.mu.Unlock()

		return e, m, err
	}
}

func (b *backend) Stat(ctx context.Context, target string) (racer.Metadata, error) {
	e, m, err := b.metadata(ctx, target)
	if err != nil {
		return racer.Metadata{}, err
	}

	return racer.Metadata{Size: m.size, ETag: e.object.etag, TTL: &b.ttl}, nil
}

func (b *backend) Open(ctx context.Context, target, etag string) (racer.Source, error) {
	e, m, err := b.metadata(ctx, target)
	if err != nil {
		return nil, err
	}

	if etag != e.object.etag {
		return nil, racer.ErrVersionChanged
	}

	select {
	case b.slots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	var buffer []byte
	select {
	case buffer = <-b.buffers:
	default:
		buffer = make([]byte, racer.PageSize)
	}

	return &originSource{backend: b, ctx: ctx, object: e.object, meta: m, buffer: buffer, start: -1}, nil
}

type originSource struct {
	backend *backend
	ctx     context.Context
	object  objectSpec
	meta    blobMetadata
	mu      sync.Mutex
	buffer  []byte
	start   int64
	valid   int
	closed  bool
}

func (s *originSource) ReadAt(p []byte, off int64) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return 0, fs.ErrClosed
	}

	if off < 0 {
		return 0, fmt.Errorf("negative offset")
	}

	var n int

	for len(p) > 0 {
		if err := s.ctx.Err(); err != nil {
			return n, err
		}

		if off >= s.meta.size {
			return n, io.EOF
		}

		start := off / racer.PageSize * racer.PageSize
		if s.start != start {
			length := int(min(racer.PageSize, s.meta.size-start))

			s.start = -1
			if err := s.backend.source.read(s.ctx, s.object, s.meta, s.buffer[:length], start); err != nil {
				return n, err
			}

			s.start, s.valid = start, length
		}

		copied := copy(p, s.buffer[off-s.start:int64(s.valid)])
		n += copied
		off += int64(copied)
		p = p[copied:]
	}

	return n, nil
}

func (s *originSource) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.closed {
		s.closed = true
		s.backend.buffers <- s.buffer

		s.buffer = nil
		<-s.backend.slots
	}

	return nil
}
