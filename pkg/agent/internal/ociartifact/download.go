// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package ociartifact

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"runtime"
	"strings"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/retry"

	"github.com/Azure/unbounded/internal/ociutil"
)

// Open opens one OCI artifact blob selected by title from a source of the form
// oci://registry/repo:tag#path/to/blob. It resolves and reads the artifact
// manifest metadata for the source, then returns a reader for only the selected
// blob. It does not pull or materialize the whole OCI artifact bundle.
func Open(ctx context.Context, source string) (io.ReadCloser, error) {
	parsed, title, err := parseBlobSource(source)
	if err != nil {
		return nil, err
	}

	repo, reference, err := openRepository(parsed)
	if err != nil {
		return nil, err
	}

	manifest, err := fetchManifest(ctx, repo, reference, source)
	if err != nil {
		return nil, err
	}

	desc, ok := DescriptorsByTitle(manifest)[title]
	if !ok {
		return nil, fmt.Errorf("OCI artifact %q does not contain blob %q", source, title)
	}

	body, err := repo.Fetch(ctx, desc)
	if err != nil {
		return nil, fmt.Errorf("fetch OCI blob %q: %w", title, err)
	}

	return body, nil
}

// FetchManifest resolves an OCI artifact source and returns its selected
// platform manifest. The source must be of the form oci://registry/repo:tag.
func FetchManifest(ctx context.Context, sourceRoot string) (ocispec.Manifest, error) {
	parsed, err := parseArtifactSource(sourceRoot)
	if err != nil {
		return ocispec.Manifest{}, err
	}

	repo, reference, err := openRepository(parsed)
	if err != nil {
		return ocispec.Manifest{}, err
	}

	return fetchManifest(ctx, repo, reference, sourceRoot)
}

// DescriptorsByTitle indexes an OCI artifact manifest's layers by title
// annotation.
func DescriptorsByTitle(manifest ocispec.Manifest) map[string]ocispec.Descriptor {
	out := make(map[string]ocispec.Descriptor, len(manifest.Layers))

	for _, desc := range manifest.Layers {
		title := desc.Annotations[ocispec.AnnotationTitle]
		if title != "" {
			out[title] = desc
		}
	}

	return out
}

func parseBlobSource(source string) (*url.URL, string, error) {
	parsed, err := parseArtifactSource(source)
	if err != nil {
		return nil, "", err
	}

	title := strings.TrimPrefix(parsed.Fragment, "/")
	if title == "" {
		return nil, "", fmt.Errorf("OCI download source must include a blob title fragment")
	}

	return parsed, title, nil
}

func parseArtifactSource(source string) (*url.URL, error) {
	parsed, err := url.Parse(source)
	if err != nil {
		return nil, fmt.Errorf("parse OCI artifact source %q: %w", source, err)
	}

	if parsed.Scheme != "oci" {
		return nil, fmt.Errorf("OCI artifact source must use oci:// scheme: %q", source)
	}

	if parsed.Host == "" || strings.Trim(parsed.Path, "/") == "" {
		return nil, fmt.Errorf("OCI artifact source must include registry and repository")
	}

	return parsed, nil
}

func fetchManifest(ctx context.Context, repo *remote.Repository, reference, sourceRoot string) (ocispec.Manifest, error) {
	desc, err := repo.Resolve(ctx, reference)
	if err != nil {
		return ocispec.Manifest{}, fmt.Errorf("resolve OCI artifact %q: %w", sourceRoot, err)
	}

	return fetchManifestDescriptor(ctx, repo, sourceRoot, desc)
}

func fetchManifestDescriptor(ctx context.Context, repo *remote.Repository, sourceRoot string, desc ocispec.Descriptor) (ocispec.Manifest, error) {
	body, err := repo.Fetch(ctx, desc)
	if err != nil {
		return ocispec.Manifest{}, fmt.Errorf("fetch OCI artifact manifest %q: %w", sourceRoot, err)
	}
	defer body.Close() //nolint:errcheck // best effort close

	data, err := io.ReadAll(body)
	if err != nil {
		return ocispec.Manifest{}, fmt.Errorf("read OCI artifact manifest %q: %w", sourceRoot, err)
	}

	if isIndexMediaType(desc.MediaType) {
		return fetchPlatformManifest(ctx, repo, sourceRoot, data)
	}

	var manifest ocispec.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return ocispec.Manifest{}, fmt.Errorf("decode OCI artifact manifest %q: %w", sourceRoot, err)
	}

	if isIndexMediaType(manifest.MediaType) {
		return fetchPlatformManifest(ctx, repo, sourceRoot, data)
	}

	return manifest, nil
}

func isIndexMediaType(mediaType string) bool {
	return mediaType == ocispec.MediaTypeImageIndex || mediaType == ociutil.DockerMediaTypeManifestList
}

func fetchPlatformManifest(ctx context.Context, repo *remote.Repository, sourceRoot string, data []byte) (ocispec.Manifest, error) {
	var index ocispec.Index
	if err := json.Unmarshal(data, &index); err != nil {
		return ocispec.Manifest{}, fmt.Errorf("decode OCI artifact index %q: %w", sourceRoot, err)
	}

	platformDesc, err := selectPlatformManifest(sourceRoot, index)
	if err != nil {
		return ocispec.Manifest{}, err
	}

	return fetchManifestDescriptor(ctx, repo, sourceRoot, platformDesc)
}

func selectPlatformManifest(sourceRoot string, index ocispec.Index) (ocispec.Descriptor, error) {
	available := make([]string, 0, len(index.Manifests))
	for _, manifestDesc := range index.Manifests {
		if manifestDesc.Platform == nil {
			available = append(available, "<unknown>")
			continue
		}

		platform := manifestDesc.Platform
		available = append(available, platform.OS+"/"+platform.Architecture)

		if platform.OS == runtime.GOOS && platform.Architecture == runtime.GOARCH {
			return manifestDesc, nil
		}
	}

	return ocispec.Descriptor{}, fmt.Errorf("OCI artifact %q does not contain platform %s/%s (available: %s)", sourceRoot, runtime.GOOS, runtime.GOARCH, strings.Join(available, ", "))
}

func openRepository(parsed *url.URL) (*remote.Repository, string, error) {
	ref := parsed.Host + parsed.Path

	name, reference, err := splitReference(ref)
	if err != nil {
		return nil, "", err
	}

	repo, err := remote.NewRepository(name)
	if err != nil {
		return nil, "", fmt.Errorf("parse OCI repository %q: %w", name, err)
	}

	ociutil.ConfigurePlainHTTP(repo)
	repo.Client = &auth.Client{Client: retry.DefaultClient, Cache: auth.DefaultCache}

	return repo, reference, nil
}

func splitReference(ref string) (name, reference string, err error) {
	if idx := strings.LastIndex(ref, "@"); idx != -1 {
		return ref[:idx], ref[idx+1:], nil
	}

	lastSlash := strings.LastIndex(ref, "/")
	lastColon := strings.LastIndex(ref, ":")

	if lastColon > lastSlash && lastColon != -1 {
		return ref[:lastColon], ref[lastColon+1:], nil
	}

	return "", "", fmt.Errorf("OCI artifact source %q must include a tag or digest", ref)
}
