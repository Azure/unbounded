// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package ociartifact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"runtime"
	"strings"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/retry"

	"github.com/Azure/unbounded/internal/ociutil"
	"github.com/Azure/unbounded/pkg/agent/internal/utilio"
)

// Open opens an OCI artifact blob selected by title from a source of the form
// oci://registry/repo:tag#path/to/blob.
func Open(ctx context.Context, source string) (io.ReadCloser, error) {
	parsed, err := url.Parse(source)
	if err != nil {
		return nil, fmt.Errorf("parse OCI download source %q: %w", source, err)
	}

	if parsed.Scheme != "oci" {
		return nil, fmt.Errorf("OCI download source must use oci:// scheme: %q", source)
	}

	title := strings.TrimPrefix(parsed.Fragment, "/")
	if title == "" {
		return nil, fmt.Errorf("OCI download source must include a blob title fragment")
	}

	repo, reference, err := openRepository(parsed)
	if err != nil {
		return nil, err
	}

	manifest, err := fetchManifest(ctx, repo, reference)
	if err != nil {
		return nil, err
	}

	for _, desc := range manifest.Layers {
		if desc.Annotations[ocispec.AnnotationTitle] == title {
			body, err := repo.Fetch(ctx, desc)
			if err != nil {
				return nil, fmt.Errorf("fetch OCI blob %q: %w", title, err)
			}

			return body, nil
		}
	}

	return nil, fmt.Errorf("OCI artifact does not contain blob %q", title)
}

// Probe verifies that an OCI artifact blob can be opened.
func Probe(ctx context.Context, source string) error {
	body, err := Open(ctx, source)
	if err != nil {
		return err
	}

	return body.Close()
}

// DownloadToLocalFile downloads an OCI artifact blob to a local file.
func DownloadToLocalFile(ctx context.Context, source, filename string, perm os.FileMode) error {
	body, err := Open(ctx, source)
	if err != nil {
		return err
	}
	defer body.Close() //nolint:errcheck // body close

	return utilio.InstallFile(filename, body, perm)
}

// DownloadWithSHA256Verification downloads an OCI artifact blob and verifies it
// against another OCI artifact blob containing a SHA256 checksum.
func DownloadWithSHA256Verification(ctx context.Context, source, checksumSource, filename string, perm os.FileMode) error {
	expectedHash, err := fetchSHA256(ctx, checksumSource)
	if err != nil {
		return fmt.Errorf("fetch checksum from %q: %w", checksumSource, err)
	}

	body, err := Open(ctx, source)
	if err != nil {
		return err
	}
	defer body.Close() //nolint:errcheck // body close

	hasher := sha256.New()
	teeReader := io.TeeReader(body, hasher)

	if err := utilio.InstallFile(filename, teeReader, perm); err != nil {
		return err
	}

	actualHash := hex.EncodeToString(hasher.Sum(nil))
	if actualHash != expectedHash {
		_ = os.Remove(filename) //nolint:errcheck // best-effort cleanup
		return fmt.Errorf("SHA256 mismatch for %q: expected %s, got %s", source, expectedHash, actualHash)
	}

	return nil
}

// DecompressTarGzFromRemote returns an iterator over files in a gzip-compressed
// tar archive stored as an OCI artifact blob.
func DecompressTarGzFromRemote(ctx context.Context, source string) utilio.TarFileSeq {
	return func(yield func(*utilio.TarFile, error) bool) {
		body, err := Open(ctx, source)
		if err != nil {
			yield(nil, err)
			return
		}
		defer body.Close() //nolint:errcheck // body close

		for tarFile, err := range utilio.DecompressTarGz(body) {
			if !yield(tarFile, err) {
				return
			}
		}
	}
}

func fetchSHA256(ctx context.Context, source string) (string, error) {
	body, err := Open(ctx, source)
	if err != nil {
		return "", err
	}
	defer body.Close() //nolint:errcheck // body close

	raw, err := io.ReadAll(io.LimitReader(body, 1024))
	if err != nil {
		return "", fmt.Errorf("read checksum body: %w", err)
	}

	hashStr := strings.TrimSpace(string(raw))
	if fields := strings.Fields(hashStr); len(fields) >= 1 {
		hashStr = fields[0]
	}

	if len(hashStr) != sha256.Size*2 {
		return "", fmt.Errorf("invalid SHA256 hash length %d in checksum file", len(hashStr))
	}

	if _, err := hex.DecodeString(hashStr); err != nil {
		return "", fmt.Errorf("invalid hex in checksum file: %w", err)
	}

	return hashStr, nil
}

func fetchManifest(ctx context.Context, repo *remote.Repository, reference string) (ocispec.Manifest, error) {
	desc, err := repo.Resolve(ctx, reference)
	if err != nil {
		return ocispec.Manifest{}, fmt.Errorf("resolve OCI artifact: %w", err)
	}

	return fetchManifestDescriptor(ctx, repo, desc)
}

func fetchManifestDescriptor(ctx context.Context, repo *remote.Repository, desc ocispec.Descriptor) (ocispec.Manifest, error) {
	body, err := repo.Fetch(ctx, desc)
	if err != nil {
		return ocispec.Manifest{}, fmt.Errorf("fetch OCI artifact manifest: %w", err)
	}
	defer body.Close() //nolint:errcheck // best effort close

	data, err := io.ReadAll(body)
	if err != nil {
		return ocispec.Manifest{}, fmt.Errorf("read OCI artifact manifest: %w", err)
	}

	if desc.MediaType == ocispec.MediaTypeImageIndex {
		return fetchPlatformManifest(ctx, repo, data)
	}

	var manifest ocispec.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return ocispec.Manifest{}, fmt.Errorf("decode OCI artifact manifest: %w", err)
	}

	if manifest.MediaType == ocispec.MediaTypeImageIndex {
		return fetchPlatformManifest(ctx, repo, data)
	}

	return manifest, nil
}

func fetchPlatformManifest(ctx context.Context, repo *remote.Repository, data []byte) (ocispec.Manifest, error) {
	var index ocispec.Index
	if err := json.Unmarshal(data, &index); err != nil {
		return ocispec.Manifest{}, fmt.Errorf("decode OCI artifact index: %w", err)
	}

	platformDesc, err := selectPlatformManifest(index)
	if err != nil {
		return ocispec.Manifest{}, err
	}

	return fetchManifestDescriptor(ctx, repo, platformDesc)
}

func selectPlatformManifest(index ocispec.Index) (ocispec.Descriptor, error) {
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

	return ocispec.Descriptor{}, fmt.Errorf("OCI artifact index does not contain platform %s/%s (available: %s)", runtime.GOOS, runtime.GOARCH, strings.Join(available, ", "))
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

	return "", "", fmt.Errorf("OCI download source %q must include a tag or digest", ref)
}
