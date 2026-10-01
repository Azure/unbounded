// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
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

func sdkClientConfig(opts pullOptions, capacity int) (racersdk.ClientConfig, error) {
	cache, err := racersdk.ParseCacheName(opts.Cache)
	if err != nil {
		return racersdk.ClientConfig{}, fmt.Errorf("cache: %w", err)
	}
	// Never let the SDK's zero/default 64 silently cap the configured workload.
	blobConcurrency := opts.BlobConcurrency
	if blobConcurrency == 0 {
		blobConcurrency = opts.LayerConcurrency
	}

	limit := max(1, capacity*blobConcurrency)

	return racersdk.ClientConfig{
		Cache: cache, MaxConnections: limit, MaxQueuedRequests: limit,
		QueueTimeout: opts.Timeout, ResponseHeaderTimeout: opts.Timeout, BodyReadTimeout: opts.Timeout,
	}, nil
}

func (p *puller) configureUDS(capacity int) error {
	config, err := sdkClientConfig(p.opts, capacity)
	if err != nil {
		return err
	}

	client, err := racersdk.NewClient(config)
	if err != nil {
		return err
	}

	p.closeBackend = client.Close
	p.acquire = udsAcquirer(client)

	return nil
}

func udsAcquirer(client *racersdk.Client) func(context.Context, string, ocispec.Descriptor) (blobResponse, error) {
	return func(ctx context.Context, _ string, desc ocispec.Descriptor) (blobResponse, error) {
		key, err := racersdk.ParseKey(desc.Digest.Encoded())
		if err != nil {
			return blobResponse{}, err
		}

		pin, err := racersdk.ParseETag(`"` + desc.Digest.String() + `"`)
		if err != nil {
			return blobResponse{}, err
		}

		value, err := client.Get(ctx, racersdk.Request{Key: key}, racersdk.ReadOptions{Pin: pin})
		if err != nil {
			return blobResponse{}, err
		}

		size := int64(value.Metadata().Size)

		return blobResponse{body: value, success: true, totalSize: &size}, nil
	}
}
