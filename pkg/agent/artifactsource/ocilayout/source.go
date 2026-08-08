// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package ocilayout

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"oras.land/oras-go/v2/content/oci"

	"github.com/Azure/unbounded/pkg/agent/artifactsource"
	"github.com/Azure/unbounded/pkg/agent/internal/utilio"
)

const (
	ociLayoutScheme  = "oci-layout://"
	ociImageScheme   = "oci://"
	httpsImageScheme = "https://"
)

// Probe checks that an OCI image source is reachable without pulling image
// contents.
func Probe(ctx context.Context, image string) error {
	if archiveURL, ok, err := parseHTTPSArchiveReference(image); ok || err != nil {
		if err != nil {
			return fmt.Errorf("parse HTTPS OCI archive reference: %w", err)
		}

		source, err := artifactsource.Parse(archiveURL)
		if err != nil {
			return fmt.Errorf("parse HTTPS OCI archive source: %w", err)
		}

		if err := source.Probe(ctx); err != nil {
			return fmt.Errorf("probe HTTPS OCI archive: %w", err)
		}

		return nil
	}

	if layoutDir, reference, ok, err := parseOCILayoutReference(image); ok || err != nil {
		if err != nil {
			return fmt.Errorf("parse OCI layout reference: %w", err)
		}

		store, err := oci.New(layoutDir)
		if err != nil {
			return fmt.Errorf("open OCI layout: %w", err)
		}

		if _, err := store.Resolve(ctx, reference); err != nil {
			return fmt.Errorf("resolve OCI layout image manifest: %w", err)
		}

		return nil
	}

	repository, reference, err := parseImageReference(image)
	if err != nil {
		return fmt.Errorf("parse image reference: %w", err)
	}

	repo, err := newRemoteRepository(repository)
	if err != nil {
		return fmt.Errorf("connect to remote repository: %w", err)
	}

	if _, err := repo.Resolve(ctx, reference); err != nil {
		return fmt.Errorf("resolve image manifest: %w", err)
	}

	return nil
}

func parseHTTPSArchiveReference(image string) (archiveURL string, ok bool, err error) {
	if !strings.HasPrefix(image, httpsImageScheme) {
		return "", false, nil
	}

	parsed, err := url.Parse(image)
	if err != nil {
		return "", true, utilio.RedactHTTPError(err)
	}

	if parsed.Host == "" || strings.Trim(parsed.Path, "/") == "" {
		return "", true, fmt.Errorf("HTTPS OCI archive reference must include a host and archive path")
	}

	if parsed.User != nil || parsed.Fragment != "" {
		return "", true, fmt.Errorf("HTTPS OCI archive reference must not include user info or a fragment")
	}

	parsed.Path = strings.TrimRight(parsed.Path, "/")
	parsed.RawPath = strings.TrimRight(parsed.RawPath, "/")

	return parsed.String(), true, nil
}

func parseOCILayoutReference(image string) (layoutDir, reference string, ok bool, err error) {
	if !strings.HasPrefix(image, ociLayoutScheme) {
		return "", "", false, nil
	}

	source := strings.TrimPrefix(image, ociLayoutScheme)
	if source == "" {
		return "", "", true, fmt.Errorf("empty OCI layout reference")
	}

	lastSlash := strings.LastIndex(source, "/")
	lastColon := strings.LastIndex(source, ":")

	if lastColon > lastSlash {
		layoutDir = source[:lastColon]
		reference = source[lastColon+1:]

		if layoutDir == "" || reference == "" {
			return "", "", true, fmt.Errorf("invalid OCI layout reference")
		}

		return layoutDir, reference, true, nil
	}

	return source, "latest", true, nil
}

func parseImageReference(image string) (repository, reference string, err error) {
	image = strings.TrimPrefix(image, ociImageScheme)
	if image == "" {
		return "", "", fmt.Errorf("empty image reference")
	}

	if idx := strings.LastIndex(image, "@"); idx != -1 {
		return image[:idx], image[idx+1:], nil
	}

	lastSlash := strings.LastIndex(image, "/")
	lastColon := strings.LastIndex(image, ":")

	if lastColon > lastSlash && lastColon != -1 {
		return image[:lastColon], image[lastColon+1:], nil
	}

	return image, "latest", nil
}
