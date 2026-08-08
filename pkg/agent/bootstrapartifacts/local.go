// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package bootstrapartifacts

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"

	"github.com/Azure/unbounded/pkg/agent/artifactsource"
)

type localBundle struct {
	root string
}

func openLocalBundle(raw string, parsed *url.URL) (Bundle, error) {
	root := raw

	if parsed.Scheme == "file" {
		if parsed.Host != "" && parsed.Host != "localhost" {
			return nil, fmt.Errorf("file artifact bundle source must not include host %q", parsed.Host)
		}

		unescaped, err := url.PathUnescape(parsed.Path)
		if err != nil {
			return nil, fmt.Errorf("unescape file artifact bundle path: %w", err)
		}

		root = unescaped
	}

	if !filepath.IsAbs(root) {
		return nil, fmt.Errorf("local artifact bundle source must use an absolute path: %q", raw)
	}

	return &localBundle{root: filepath.Clean(root)}, nil
}

func (b *localBundle) Root() string {
	return b.root
}

func (b *localBundle) Artifact(path string) (artifactsource.Source, error) {
	cleaned, err := cleanArtifactPath(path)
	if err != nil {
		return artifactsource.Source{}, err
	}

	return artifactsource.Parse(filepath.Join(b.root, cleaned))
}

func (b *localBundle) ArtifactURL(path string) string {
	rootURL := (&url.URL{Scheme: "file", Path: filepath.ToSlash(b.root)}).String()

	return rootURL + "/" + path
}

func (b *localBundle) List(_ context.Context) ([]string, error) {
	var paths []string

	if err := filepath.WalkDir(b.root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if entry.IsDir() {
			return nil
		}

		info, err := entry.Info()
		if err != nil {
			return err
		}

		if !info.Mode().IsRegular() {
			return nil
		}

		relative, err := filepath.Rel(b.root, path)
		if err != nil {
			return err
		}

		paths = append(paths, filepath.ToSlash(relative))

		return nil
	}); err != nil {
		return nil, fmt.Errorf("list local artifact bundle %q: %w", b.root, err)
	}

	sort.Strings(paths)

	return paths, nil
}
