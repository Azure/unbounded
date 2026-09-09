// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Command acl-sysext-publisher pushes a built Azure Container Linux nspawn
// system extension to an OCI registry as a titled-blob artifact.
//
// The agent resolves the extension through artifactsource's
// oci://registry/repo:tag#blob-title form, so each file in the build directory
// is pushed as a layer whose title is its file name.
//
// The artifact identity is a function of the systemd build the extension
// targets, not of the unbounded release, so callers are expected to tag by
// systemd version and architecture. See images/acl-nspawn-sysext/README.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content/file"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/credentials"
	"oras.land/oras-go/v2/registry/remote/retry"

	"github.com/Azure/unbounded/internal/ociutil"
)

const (
	// artifactType distinguishes this from the bootstrap-artifacts bundles
	// published by agent-artifacts-builder.
	artifactType = "application/vnd.unbounded.acl-nspawn-sysext.v1"

	// fileMediaType matches what agent-artifacts-builder uses for its layers.
	fileMediaType = "application/octet-stream"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		dir  string
		ref  string
		arch string
	)

	flag.StringVar(&dir, "dir", "", "directory holding the built extension (required)")
	flag.StringVar(&ref, "ref", "", "OCI reference to push to, for example ghcr.io/org/repo:255-33.azl3-amd64 (required)")
	flag.StringVar(&arch, "arch", "", "architecture the extension was built for, recorded as an annotation (required)")
	flag.Parse()

	if dir == "" || ref == "" || arch == "" {
		flag.Usage()

		return errors.New("--dir, --ref and --arch are required")
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	return push(context.Background(), log, dir, ref, arch)
}

func push(ctx context.Context, log *slog.Logger, dir, ref, arch string) error {
	ref = strings.TrimPrefix(ref, "oci://")

	names, err := artifactFiles(dir)
	if err != nil {
		return err
	}

	repo, err := remote.NewRepository(ref)
	if err != nil {
		return fmt.Errorf("parse OCI reference %q: %w", ref, err)
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

	store, err := file.New(dir)
	if err != nil {
		return fmt.Errorf("open file store %q: %w", dir, err)
	}

	defer store.Close() //nolint:errcheck // best effort close

	layers := make([]ocispec.Descriptor, 0, len(names))

	for _, name := range names {
		// The third argument becomes the layer title, which is how the agent
		// addresses an individual blob.
		desc, addErr := store.Add(ctx, name, fileMediaType, name)
		if addErr != nil {
			return fmt.Errorf("add %q to the artifact: %w", name, addErr)
		}

		log.Info("added blob", slog.String("title", name), slog.String("digest", desc.Digest.String()))

		layers = append(layers, desc)
	}

	manifestDesc, err := oras.PackManifest(ctx, store, oras.PackManifestVersion1_1, artifactType, oras.PackManifestOptions{
		Layers: layers,
		ManifestAnnotations: map[string]string{
			"org.opencontainers.image.description": "systemd-container system extension for Azure Container Linux",
			"io.unbounded.sysext.arch":             arch,
		},
	})
	if err != nil {
		return fmt.Errorf("pack OCI manifest: %w", err)
	}

	manifestDesc.ArtifactType = artifactType

	tag := repo.Reference.Reference
	if err := store.Tag(ctx, manifestDesc, tag); err != nil {
		return fmt.Errorf("tag OCI artifact %q: %w", tag, err)
	}

	desc, err := oras.Copy(ctx, store, tag, repo, tag, oras.DefaultCopyOptions)
	if err != nil {
		return fmt.Errorf("push OCI artifact %q: %w", ref, err)
	}

	log.Info("pushed system extension",
		slog.String("oci_ref", "oci://"+ref),
		slog.String("digest", desc.Digest.String()),
	)

	return nil
}

// artifactFiles returns the regular files to publish, sorted so the resulting
// manifest is stable across runs.
//
// The extension is a flat set of files, so a nested directory means the build
// produced something unexpected and is treated as an error rather than being
// silently skipped.
func artifactFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read artifact directory %q: %w", dir, err)
	}

	var names []string

	for _, entry := range entries {
		if entry.IsDir() {
			return nil, fmt.Errorf("unexpected directory %q in artifact directory %q", entry.Name(), dir)
		}

		names = append(names, entry.Name())
	}

	if len(names) == 0 {
		return nil, fmt.Errorf("artifact directory %q is empty", dir)
	}

	sort.Strings(names)

	return names, nil
}
