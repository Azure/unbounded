// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"hash/fnv"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/Azure/unbounded/pkg/racersdk"
)

type blobResponse struct {
	body    io.ReadCloser
	status  int
	success bool
	// SDK metadata describes the total object, independently of the stream length.
	totalSize *int64
	etag      *string
}

func (p *puller) acquireHTTP(ctx context.Context, kind string, desc ocispec.Descriptor) (blobResponse, error) {
	endpoint := *p.target

	resource := "blobs"
	if kind == "manifest" {
		resource = "manifests"
	}

	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/v2/" + p.img.repository + "/" + resource + "/" + desc.Digest.String()

	endpoint.RawPath = ""
	if p.opts.Namespace != "" {
		endpoint.RawQuery = url.Values{"ns": {p.opts.Namespace}}.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return blobResponse{}, err
	}

	req.Header.Set("Accept", desc.MediaType)

	response, err := p.client.Do(req)
	if err != nil {
		return blobResponse{}, err
	}

	return blobResponse{body: response.Body, status: response.StatusCode, success: response.StatusCode == http.StatusOK}, nil
}

func sdkClientConfig(opts pullOptions, capacity int) racersdk.ClientConfig {
	// Never let the SDK's zero/default 64 silently cap the configured workload.
	limit := max(1, capacity*opts.BlobConcurrency)

	return racersdk.ClientConfig{Cache: opts.Cache, MaxConnections: limit, PageCredits: opts.PageCredits}
}

func (p *puller) configureUDS(capacity int) error {
	client, err := racersdk.NewClient(sdkClientConfig(p.opts, capacity))
	if err != nil {
		return err
	}

	p.closeBackend = client.Close
	p.acquire = udsAcquirer(client)
	if p.opts.RangeBytes > 0 {
		p.acquire = p.udsRangedAcquirer(client)
	}

	return nil
}

func udsAcquirer(client *racersdk.Client) func(context.Context, string, ocispec.Descriptor) (blobResponse, error) {
	return func(ctx context.Context, _ string, desc ocispec.Descriptor) (blobResponse, error) {
		key, err := racersdk.ParseKey(desc.Digest.Encoded())
		if err != nil {
			return blobResponse{}, err
		}

		// Match Gantry's ordinary full-object read: let Racer select fresh
		// metadata, then validate it. Preserve Object.WriteTo for unverified reads.
		value, err := client.Get(ctx, racersdk.Request{Key: key})
		if err != nil {
			return blobResponse{}, err
		}

		size := value.Metadata().Size
		etag := value.Metadata().ETag

		return blobResponse{body: value, success: true, totalSize: &size, etag: &etag}, nil
	}
}

// udsRangedAcquirer reads large blobs as pinned, resumable, parallel ranges.
func (p *puller) udsRangedAcquirer(client *racersdk.Client) func(context.Context, string, ocispec.Descriptor) (blobResponse, error) {
	return func(ctx context.Context, _ string, desc ocispec.Descriptor) (blobResponse, error) {
		key, err := racersdk.ParseKey(desc.Digest.Encoded())
		if err != nil {
			return blobResponse{}, err
		}

		metadata, err := client.Stat(ctx, racersdk.Request{Key: key})
		if err != nil {
			return blobResponse{}, err
		}

		size, etag := metadata.Size, metadata.ETag
		body := &rangedBody{
			ctx:        ctx,
			get:        sdkRangeGet(client, key, etag),
			size:       size,
			rangeBytes: p.opts.RangeBytes,
			parallel:   p.opts.RangeParallel,
			retries:    p.opts.RangeRetries,
			retryDelay: p.opts.RetryDelay,
			rotate:     p.rangeRotation(size),
			onResume: func() {
				p.metrics.rangeResumes.Inc()
			},
		}

		return blobResponse{body: body, success: true, totalSize: &size, etag: &etag}, nil
	}
}

// rangeRotation picks this process's first range from its host name when
// range-rotate is set, spreading concurrent readers across the object.
func (p *puller) rangeRotation(size int64) int64 {
	if !p.opts.RangeRotate || p.opts.RangeBytes <= 0 {
		return 0
	}

	name, err := os.Hostname()
	if err != nil {
		return 0
	}

	h := fnv.New64a()
	_, _ = h.Write([]byte(name)) //nolint:errcheck // hash.Hash never fails.

	ranges := (size + p.opts.RangeBytes - 1) / p.opts.RangeBytes

	return int64(h.Sum64() % uint64(ranges))
}
