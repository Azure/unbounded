// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package ocilayout acquires OCI images as local OCI image layouts.
package ocilayout

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content/oci"
	"oras.land/oras-go/v2/registry"

	"github.com/Azure/unbounded/pkg/agent/artifactsource"
)

// Layout is an acquired local OCI image layout.
type Layout struct {
	Dir       string
	Reference string
	cleanup   func()
}

// Close releases temporary layout state. It is a no-op for local layouts.
func (l *Layout) Close() error {
	if l != nil && l.cleanup != nil {
		l.cleanup()
		l.cleanup = nil
	}

	return nil
}

// Acquire resolves source into a local OCI image layout.
func Acquire(ctx context.Context, source string) (*Layout, error) {
	if archiveURL, ok, err := parseHTTPSArchiveReference(source); ok || err != nil {
		if err != nil {
			return nil, fmt.Errorf("parse HTTPS OCI archive reference: %w", err)
		}

		return acquireHTTPSArchive(ctx, archiveURL)
	}

	if layoutDir, reference, ok, err := parseOCILayoutReference(source); ok || err != nil {
		if err != nil {
			return nil, fmt.Errorf("parse OCI layout reference: %w", err)
		}

		return &Layout{Dir: layoutDir, Reference: reference}, nil
	}

	return acquireRegistryImage(ctx, source)
}

func acquireHTTPSArchive(ctx context.Context, archiveURL string) (*Layout, error) {
	extractDir, err := os.MkdirTemp("", "unbounded-oci-archive-*")
	if err != nil {
		return nil, fmt.Errorf("create OCI archive extraction directory: %w", err)
	}

	cleanup := func() {
		os.RemoveAll(extractDir) //nolint:errcheck // best effort cleanup
	}

	source, err := artifactsource.Parse(archiveURL)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("parse HTTPS OCI archive source: %w", err)
	}

	if err := source.ExtractTar(ctx, extractDir); err != nil {
		cleanup()
		return nil, fmt.Errorf("download and extract HTTPS OCI image archive: %w", err)
	}

	layoutDir, err := findOCILayoutRoot(extractDir)
	if err != nil {
		cleanup()
		return nil, err
	}

	reference, err := singleOCILayoutReference(layoutDir)
	if err != nil {
		cleanup()
		return nil, err
	}

	return &Layout{Dir: layoutDir, Reference: reference, cleanup: cleanup}, nil
}

func acquireRegistryImage(ctx context.Context, image string) (*Layout, error) {
	repository, reference, err := parseImageReference(image)
	if err != nil {
		return nil, fmt.Errorf("parse image reference: %w", err)
	}

	layoutDir, err := os.MkdirTemp("", "unbounded-oci-*")
	if err != nil {
		return nil, fmt.Errorf("create temporary OCI layout: %w", err)
	}

	cleanup := func() {
		os.RemoveAll(layoutDir) //nolint:errcheck // best effort cleanup
	}

	store, err := oci.New(layoutDir)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("create OCI layout store: %w", err)
	}

	repo, err := newRemoteRepository(repository)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("connect to remote repository %q: %w", repository, err)
	}

	if _, err := oras.Copy(ctx, repo, reference, store, reference, oras.DefaultCopyOptions); err != nil {
		cleanup()
		return nil, fmt.Errorf("pull image %s:%s: %w", repository, reference, err)
	}

	return &Layout{Dir: layoutDir, Reference: reference, cleanup: cleanup}, nil
}

func findOCILayoutRoot(extractDir string) (string, error) {
	var layouts []string

	if err := filepath.WalkDir(extractDir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if entry.IsDir() && entry.Name() == "blobs" {
			return filepath.SkipDir
		}

		if !entry.IsDir() && entry.Name() == "oci-layout" {
			layouts = append(layouts, filepath.Dir(path))
		}

		return nil
	}); err != nil {
		return "", fmt.Errorf("inspect HTTPS OCI image archive: %w", err)
	}

	if len(layouts) == 0 {
		return "", fmt.Errorf("HTTPS OCI image archive does not contain an OCI layout")
	}

	if len(layouts) > 1 {
		return "", fmt.Errorf("HTTPS OCI image archive contains multiple OCI layouts")
	}

	indexInfo, err := os.Stat(filepath.Join(layouts[0], "index.json"))
	if err != nil || !indexInfo.Mode().IsRegular() {
		return "", fmt.Errorf("HTTPS OCI image archive does not contain a regular index.json")
	}

	return layouts[0], nil
}

func singleOCILayoutReference(layoutDir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(layoutDir, "index.json"))
	if err != nil {
		return "", fmt.Errorf("read HTTPS OCI image archive index: %w", err)
	}

	var index ocispec.Index
	if err := json.Unmarshal(data, &index); err != nil {
		return "", fmt.Errorf("parse HTTPS OCI image archive index: %w", err)
	}

	var reference string

	for _, descriptor := range index.Manifests {
		name := descriptor.Annotations[ocispec.AnnotationRefName]
		if name == "" {
			continue
		}

		if reference != "" {
			return "", fmt.Errorf("HTTPS OCI image archive contains multiple tagged image references")
		}

		reference = name
	}

	if reference == "" {
		return "", fmt.Errorf("HTTPS OCI image archive does not contain a tagged image reference")
	}

	if err := (registry.Reference{Reference: reference}).ValidateReferenceAsTag(); err != nil {
		return "", fmt.Errorf("HTTPS OCI image archive reference %q is not a valid tag: %w", reference, err)
	}

	return reference, nil
}
