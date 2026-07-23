// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package artifacts

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content/oci"
	"oras.land/oras-go/v2/registry"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/credentials"
	"oras.land/oras-go/v2/registry/remote/retry"

	"github.com/Azure/unbounded/internal/ociutil"
)

const (
	ociImageCopyMaxAttempts = 5
	ociImageCopyRetryDelay  = 2 * time.Second
)

// OCIImageArchiveName returns the release archive filename for a tagged OCI
// image reference. Callers producing multiple archives must reject duplicate
// names before writing because the filename intentionally omits registry and
// repository namespace components.
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

	layoutDir, err := copyOCIImageToLayout(ctx, repo, reference)
	if err != nil {
		return fmt.Errorf("copy OCI image %q into layout: %w", sourceRef, err)
	}
	defer os.RemoveAll(layoutDir) //nolint:errcheck // best effort cleanup

	if err := writeDirectoryArchive(layoutDir, archivePath); err != nil {
		return fmt.Errorf("write OCI image layout archive: %w", err)
	}

	return nil
}

func copyOCIImageToLayout(ctx context.Context, repo *remote.Repository, reference string) (string, error) {
	var layoutDir string

	err := retryOCIImageCopy(ctx, func() error {
		if layoutDir != "" {
			os.RemoveAll(layoutDir) //nolint:errcheck // best effort cleanup before retry
			layoutDir = ""
		}

		dir, err := os.MkdirTemp("", "unbounded-rootfs-oci-layout-*")
		if err != nil {
			return fmt.Errorf("create OCI image layout directory: %w", err)
		}

		layoutDir = dir

		store, err := oci.New(layoutDir)
		if err != nil {
			return fmt.Errorf("create OCI image layout: %w", err)
		}

		if _, err := oras.Copy(ctx, repo, reference, store, reference, oras.DefaultCopyOptions); err != nil {
			return err
		}

		return nil
	}, waitForOCIImageCopyRetry)
	if err != nil {
		if layoutDir != "" {
			os.RemoveAll(layoutDir) //nolint:errcheck // best effort cleanup after failure
		}

		return "", err
	}

	return layoutDir, nil
}

func retryOCIImageCopy(
	ctx context.Context,
	copyImage func() error,
	wait func(context.Context, time.Duration) error,
) error {
	delay := ociImageCopyRetryDelay

	for attempt := 1; attempt <= ociImageCopyMaxAttempts; attempt++ {
		if err := context.Cause(ctx); err != nil {
			return err
		}

		err := copyImage()
		if err == nil {
			return nil
		}

		if !ociutil.RetryableNetworkError(err) || attempt == ociImageCopyMaxAttempts {
			return err
		}

		if err := wait(ctx, delay); err != nil {
			return err
		}

		delay *= 2
	}

	return nil
}

func waitForOCIImageCopyRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}
