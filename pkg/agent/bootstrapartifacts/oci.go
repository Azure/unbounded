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
	"github.com/Azure/unbounded/pkg/agent/internal/ociartifact"
)

type ociBundle struct {
	root string
}

func openOCIBundle(parsed *url.URL) (Bundle, error) {
	if parsed.Host == "" || strings.Trim(parsed.Path, "/") == "" {
		return nil, fmt.Errorf("OCI artifact bundle source must include registry and repository")
	}

	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("OCI artifact bundle source must not include user info, query parameters, or a fragment")
	}

	parsed.Path = strings.TrimRight(parsed.Path, "/")
	parsed.RawPath = strings.TrimRight(parsed.RawPath, "/")

	return &ociBundle{root: parsed.String()}, nil
}

func (b *ociBundle) Root() string {
	return b.root
}

func (b *ociBundle) Artifact(path string) (artifactsource.Source, error) {
	cleaned, err := cleanArtifactPath(path)
	if err != nil {
		return artifactsource.Source{}, err
	}

	return artifactsource.Parse(b.root + "#" + filepath.ToSlash(cleaned))
}

func (b *ociBundle) ArtifactURL(path string) string {
	return b.root + "#" + path
}

func (b *ociBundle) List(ctx context.Context) ([]string, error) {
	manifest, err := ociartifact.FetchManifest(ctx, b.root)
	if err != nil {
		return nil, err
	}

	descriptors := ociartifact.DescriptorsByTitle(manifest)

	paths := make([]string, 0, len(descriptors))
	for path := range descriptors {
		paths = append(paths, path)
	}

	sort.Strings(paths)

	return paths, nil
}
