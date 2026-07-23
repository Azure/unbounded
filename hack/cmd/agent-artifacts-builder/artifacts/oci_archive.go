// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package artifacts

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content/oci"
	"oras.land/oras-go/v2/registry"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/credentials"
	"oras.land/oras-go/v2/registry/remote/retry"

	"github.com/Azure/unbounded/internal/ociutil"
)

// OCIImageArchiveName returns the release archive filename for a tagged OCI
// image reference.
func OCIImageArchiveName(sourceRef string) (string, error) {
	sourceRef = strings.TrimPrefix(strings.TrimSpace(sourceRef), "oci://")

	ref, err := registry.ParseReference(sourceRef)
	if err != nil {
		return "", fmt.Errorf("parse OCI image source %q: %w", sourceRef, err)
	}

	if err := ref.ValidateReferenceAsTag(); err != nil {
		return "", fmt.Errorf("OCI image source %q must use a tag: %w", sourceRef, err)
	}

	imageName := filepath.Base(ref.Repository)

	return fmt.Sprintf("rootfs-%s-%s.oci.tar.gz", imageName, ref.Reference), nil
}

// ArchiveOCIImage copies a tagged image from an OCI registry into a local OCI
// layout and writes that layout as a gzip-compressed tar archive.
func ArchiveOCIImage(ctx context.Context, sourceRef, archivePath string) error {
	sourceRef = strings.TrimPrefix(strings.TrimSpace(sourceRef), "oci://")
	if sourceRef == "" {
		return fmt.Errorf("OCI image source is required")
	}

	if archivePath == "" {
		return fmt.Errorf("OCI image archive path is required")
	}

	repo, err := remote.NewRepository(sourceRef)
	if err != nil {
		return fmt.Errorf("parse OCI image source %q: %w", sourceRef, err)
	}

	reference := repo.Reference.Reference
	if reference == "" {
		return fmt.Errorf("OCI image source %q must include a tag", sourceRef)
	}

	if err := repo.Reference.ValidateReferenceAsTag(); err != nil {
		return fmt.Errorf("OCI image source %q must use a tag: %w", sourceRef, err)
	}

	ociutil.ConfigurePlainHTTP(repo)

	credentialStore, err := credentials.NewStoreFromDocker(credentials.StoreOptions{})
	if err != nil {
		return fmt.Errorf("load OCI registry credentials: %w", err)
	}

	repo.Client = &auth.Client{
		Client:     retry.DefaultClient,
		Cache:      auth.DefaultCache,
		Credential: credentials.Credential(credentialStore),
	}

	layoutDir, err := os.MkdirTemp("", "unbounded-rootfs-oci-layout-*")
	if err != nil {
		return fmt.Errorf("create OCI image layout directory: %w", err)
	}
	defer os.RemoveAll(layoutDir) //nolint:errcheck // best effort cleanup

	store, err := oci.New(layoutDir)
	if err != nil {
		return fmt.Errorf("create OCI image layout: %w", err)
	}

	if _, err := oras.Copy(ctx, repo, reference, store, reference, oras.DefaultCopyOptions); err != nil {
		return fmt.Errorf("copy OCI image %q into layout: %w", sourceRef, err)
	}

	if err := writeDirectoryArchive(layoutDir, archivePath); err != nil {
		return fmt.Errorf("write OCI image layout archive: %w", err)
	}

	return nil
}
