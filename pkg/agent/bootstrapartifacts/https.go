// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package bootstrapartifacts

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/Azure/unbounded/pkg/agent/artifactsource"
	"github.com/Azure/unbounded/pkg/agent/internal/utilio"
)

// ResolveOptions configures HTTPS archive-backed bundle resolution.
type ResolveOptions struct {
	CacheRoot   string
	ReadyMarker string
}

// Resolve opens a local or OCI bundle, or materializes an HTTPS archive as a
// local bundle.
func Resolve(ctx context.Context, raw string, opts ResolveOptions) (Bundle, *ArchiveCache, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil, fmt.Errorf("bootstrap artifact bundle source is empty")
	}

	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, nil, fmt.Errorf("parse bootstrap artifact bundle source: %w", utilio.RedactHTTPError(err))
	}

	if parsed.Scheme != "https" {
		bundle, err := Open(raw)

		return bundle, nil, err
	}

	if parsed.Host == "" || strings.Trim(parsed.Path, "/") == "" {
		return nil, nil, fmt.Errorf("HTTPS artifact bundle source must include a host and archive path")
	}

	if parsed.User != nil || parsed.Fragment != "" {
		return nil, nil, fmt.Errorf("HTTPS artifact bundle source must not include user info or a fragment")
	}

	parsed.Path = strings.TrimRight(parsed.Path, "/")
	parsed.RawPath = strings.TrimRight(parsed.RawPath, "/")

	source, err := artifactsource.Parse(parsed.String())
	if err != nil {
		return nil, nil, fmt.Errorf("parse HTTPS artifact bundle source: %w", err)
	}

	cache, err := MaterializeArchive(ctx, source, ArchiveCacheOptions{
		CacheRoot:   opts.CacheRoot,
		RootMarker:  ManifestFileName,
		ReadyMarker: opts.ReadyMarker,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("materialize HTTPS artifact bundle: %w", err)
	}

	bundle, err := Open(cache.Root())
	if err != nil {
		return nil, nil, fmt.Errorf("open extracted HTTPS artifact bundle: %w", err)
	}

	return bundle, cache, nil
}
