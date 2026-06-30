// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package artifacts

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content/file"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/credentials"
	"oras.land/oras-go/v2/registry/remote/retry"

	"github.com/Azure/unbounded/internal/agentartifacts"
	"github.com/Azure/unbounded/internal/ociutil"
)

// ValidateOCI pulls an offline artifact OCI bundle into a temporary staging
// directory and validates the pulled content. It verifies that the remote
// manifest has the expected artifact type, that all layers have file title
// annotations, that the pulled bundle contains a valid manifest.json, and that
// the files in the pulled bundle match the artifacts implied by manifest.json.
func ValidateOCI(ctx context.Context, log *slog.Logger, ref string) error {
	ref = strings.TrimPrefix(ref, "oci://")
	log.Info("validating offline artifact bundle", slog.String("oci_ref", "oci://"+ref))

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

	tag := repo.Reference.Reference

	manifest, err := fetchOCIManifest(ctx, repo, tag, ref)
	if err != nil {
		return err
	}

	stagingDir, cleanup, err := NewStagingDir()
	if err != nil {
		return err
	}
	defer cleanup()

	store, err := file.New(stagingDir)
	if err != nil {
		return fmt.Errorf("open file store %q: %w", stagingDir, err)
	}
	defer store.Close() //nolint:errcheck // best effort close

	if _, err := oras.Copy(ctx, repo, tag, store, tag, oras.DefaultCopyOptions); err != nil {
		return fmt.Errorf("pull OCI artifact %q: %w", ref, err)
	}

	if err := ValidateBundle(log, stagingDir); err != nil {
		return err
	}

	log.Info("validated offline artifact bundle", slog.String("oci_ref", "oci://"+ref), slog.Int("layers", len(manifest.Layers)))

	return nil
}

func fetchOCIManifest(ctx context.Context, repo *remote.Repository, tag, ref string) (ocispec.Manifest, error) {
	desc, data, err := oras.FetchBytes(ctx, repo, tag, oras.DefaultFetchBytesOptions)
	if err != nil {
		return ocispec.Manifest{}, fmt.Errorf("fetch OCI artifact manifest %q: %w", ref, err)
	}

	if desc.MediaType != ocispec.MediaTypeImageManifest {
		return ocispec.Manifest{}, fmt.Errorf("unexpected OCI manifest media type %q", desc.MediaType)
	}

	var manifest ocispec.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return ocispec.Manifest{}, fmt.Errorf("parse OCI artifact manifest: %w", err)
	}

	if manifest.ArtifactType != artifactType {
		return ocispec.Manifest{}, fmt.Errorf("unexpected OCI artifact type %q", manifest.ArtifactType)
	}

	for _, layer := range manifest.Layers {
		if layer.Annotations[ocispec.AnnotationTitle] == "" {
			return ocispec.Manifest{}, fmt.Errorf("OCI artifact layer %s is missing title annotation", layer.Digest)
		}
	}

	return manifest, nil
}

// ValidateBundle validates a local offline artifact bundle directory. It reads
// manifest.json, derives the expected artifact paths from that manifest and the
// detected architectures, compares them to the files on disk, and verifies all
// checksum sidecars.
func ValidateBundle(log *slog.Logger, rootDir string) error {
	log.Info("validating offline artifact bundle", slog.String("output_dir", rootDir))

	if err := validateBundle(rootDir); err != nil {
		return err
	}

	log.Info("validated offline artifact bundle", slog.String("output_dir", rootDir))

	return nil
}

func validateBundle(rootDir string) error {
	manifest, err := loadManifest(filepath.Join(rootDir, ManifestFileName))
	if err != nil {
		return err
	}

	manifest, err = agentartifacts.NormalizeManifest(manifest)
	if err != nil {
		return err
	}

	architectures, err := detectArchitectures(rootDir, manifest.Versions.Kubernetes)
	if err != nil {
		return err
	}

	plan, err := NewPlan(Options{
		OutputDir:     rootDir,
		Manifest:      manifest,
		Architectures: architectures,
	})
	if err != nil {
		return err
	}

	expectedPaths := expectedBundlePaths(plan)

	actualPaths, err := collectArtifactPaths(rootDir)
	if err != nil {
		return err
	}

	if !equalStrings(expectedPaths, actualPaths) {
		return fmt.Errorf("pulled OCI artifact content mismatch: got %s, want %s", strings.Join(actualPaths, ", "), strings.Join(expectedPaths, ", "))
	}

	for _, artifact := range plan.Artifacts {
		path := filepath.Join(rootDir, filepath.FromSlash(artifact.Path))
		if artifact.GenerateChecksum {
			if err := verifyChecksum(path); err != nil {
				return err
			}
		}

		if artifact.Name == "kubelet" || artifact.Name == "kubectl" || artifact.Name == "kube-proxy" {
			if err := verifyChecksum(path); err != nil {
				return err
			}
		}
	}

	return nil
}

func expectedBundlePaths(plan Plan) []string {
	paths := make([]string, 0, len(plan.Artifacts)*2+1)
	paths = append(paths, ManifestFileName)

	for _, artifact := range plan.Artifacts {
		paths = append(paths, artifact.Path)
		if artifact.GenerateChecksum {
			paths = append(paths, artifact.Path+".sha256")
		}
	}

	sort.Strings(paths)

	return paths
}

func detectArchitectures(rootDir, kubernetesVersion string) ([]string, error) {
	base := filepath.Join(rootDir, "kubernetes", kubernetesVersion, "bin", "linux")

	entries, err := os.ReadDir(base)
	if err != nil {
		return nil, fmt.Errorf("read Kubernetes artifact architecture dir %q: %w", base, err)
	}

	architectures := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			architectures = append(architectures, entry.Name())
		}
	}

	if len(architectures) == 0 {
		return nil, fmt.Errorf("no Kubernetes artifact architectures found under %q", base)
	}

	sort.Strings(architectures)

	return architectures, nil
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}

	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}

	return true
}
