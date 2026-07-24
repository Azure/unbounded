// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package bootstrapartifacts

import (
	"context"
	"fmt"
	"net/url"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Azure/unbounded/pkg/agent/artifactsource"
	"github.com/Azure/unbounded/pkg/agent/internal/utilio"
)

// Bundle provides read access to a collection of bootstrap artifacts.
type Bundle interface {
	// Root returns the normalized filesystem path or OCI reference identifying
	// the bundle root.
	Root() string

	// Artifact resolves a bundle-relative path into an openable artifact source.
	Artifact(path string) (artifactsource.Source, error)

	// ArtifactURL returns the source URL for a bundle-relative path. The path may
	// contain format verbs used later when resolving download overrides.
	ArtifactURL(path string) string

	// List returns the sorted bundle-relative paths of all available artifacts.
	// For OCI bundles, it fetches the selected platform manifest once and lists
	// descriptor titles without downloading artifact contents.
	List(ctx context.Context) ([]string, error)
}

// ContentDiff describes differences between expected and available bundle
// paths.
type ContentDiff struct {
	Missing    []string
	Unexpected []string
}

// CompareContents compares expected paths with the paths available in bundle.
func CompareContents(ctx context.Context, bundle Bundle, expected []string) (ContentDiff, error) {
	actual, err := bundle.List(ctx)
	if err != nil {
		return ContentDiff{}, err
	}

	expectedSet := make(map[string]struct{}, len(expected))
	for _, path := range expected {
		expectedSet[path] = struct{}{}
	}

	actualSet := make(map[string]struct{}, len(actual))
	for _, path := range actual {
		actualSet[path] = struct{}{}
	}

	var diff ContentDiff

	for path := range expectedSet {
		if _, ok := actualSet[path]; !ok {
			diff.Missing = append(diff.Missing, path)
		}
	}

	for path := range actualSet {
		if _, ok := expectedSet[path]; !ok {
			diff.Unexpected = append(diff.Unexpected, path)
		}
	}

	sort.Strings(diff.Missing)
	sort.Strings(diff.Unexpected)

	return diff, nil
}

// Open opens a local or OCI bootstrap artifact bundle root.
func Open(raw string) (Bundle, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("bootstrap artifact bundle source is empty")
	}

	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("parse bootstrap artifact bundle source: %w", utilio.RedactHTTPError(err))
	}

	switch parsed.Scheme {
	case "", "file":
		return openLocalBundle(raw, parsed)
	case "oci":
		return openOCIBundle(parsed)
	default:
		return nil, fmt.Errorf("unsupported bootstrap artifact bundle scheme %q", parsed.Scheme)
	}
}

func cleanArtifactPath(path string) (string, error) {
	path = filepath.FromSlash(path)
	if path == "" || !filepath.IsLocal(path) {
		return "", fmt.Errorf("artifact path must be local: %q", path)
	}

	return filepath.Clean(path), nil
}
