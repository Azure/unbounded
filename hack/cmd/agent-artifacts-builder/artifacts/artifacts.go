// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package artifacts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"golang.org/x/sync/errgroup"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content/file"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/credentials"
	"oras.land/oras-go/v2/registry/remote/retry"

	"github.com/Azure/unbounded/internal/agentartifacts"
	"github.com/Azure/unbounded/internal/ociutil"
)

const (
	ManifestFileName = agentartifacts.ManifestFileName

	artifactType  = "application/vnd.unbounded.agent.bootstrap-artifacts.v1"
	fileMediaType = "application/octet-stream"
)

type (
	Manifest = agentartifacts.Manifest
	Versions = agentartifacts.Versions
)

type Options struct {
	OutputDir    string
	OCIRef       string
	ManifestPath string
	Manifest     Manifest

	Architectures []string

	SkipExisting bool
}

type Artifact struct {
	Name string
	URL  string
	Path string
}

type Plan struct {
	Manifest  Manifest
	Artifacts []Artifact
}

func Build(ctx context.Context, opts Options) error {
	plan, err := NewPlan(opts)
	if err != nil {
		return err
	}

	if err := writeManifest(opts.OutputDir, plan.Manifest); err != nil {
		return err
	}

	if err := downloadArtifacts(ctx, opts.OutputDir, plan.Artifacts, opts.SkipExisting); err != nil {
		return err
	}

	if opts.OCIRef != "" {
		if err := PushOCI(ctx, opts.OutputDir, opts.OCIRef); err != nil {
			return err
		}
	}

	return nil
}

func NewPlan(opts Options) (Plan, error) {
	if opts.OutputDir == "" {
		return Plan{}, errors.New("output dir is required")
	}

	manifest, err := resolveManifest(opts)
	if err != nil {
		return Plan{}, err
	}

	arches := opts.Architectures
	if len(arches) == 0 {
		arches = []string{runtime.GOARCH}
	}
	for _, arch := range arches {
		if strings.TrimSpace(arch) == "" {
			return Plan{}, errors.New("architecture must not be empty")
		}
	}

	artifacts := make([]Artifact, 0, len(arches)*10)
	for _, arch := range arches {
		for _, binary := range agentartifacts.KubernetesBinaries {
			path := agentartifacts.KubernetesArtifactPath(manifest.Versions.Kubernetes, arch, binary)
			url := agentartifacts.KubernetesBinary(nil, manifest.Versions.Kubernetes, arch, binary)
			artifacts = append(artifacts, Artifact{Name: binary, URL: url, Path: path})
			artifacts = append(artifacts, Artifact{Name: binary + ".sha256", URL: url + ".sha256", Path: path + ".sha256"})
		}

		artifacts = append(artifacts,
			Artifact{
				Name: "containerd",
				URL:  agentartifacts.ContainerdArchive(nil, manifest.Versions.Containerd, arch),
				Path: agentartifacts.ContainerdArtifactPath(manifest.Versions.Containerd, arch),
			},
			Artifact{
				Name: "runc",
				URL:  agentartifacts.RuncBinary(nil, manifest.Versions.Runc, arch),
				Path: agentartifacts.RuncArtifactPath(manifest.Versions.Runc, arch),
			},
			Artifact{
				Name: "cni",
				URL:  agentartifacts.CNIPluginsArchive(nil, manifest.Versions.CNI, arch),
				Path: agentartifacts.CNIArtifactPath(manifest.Versions.CNI, arch),
			},
			Artifact{
				Name: "crictl",
				URL:  agentartifacts.CrictlArchive(nil, manifest.Versions.Crictl, "linux", arch),
				Path: agentartifacts.CrictlArtifactPath(manifest.Versions.Crictl, "linux", arch),
			},
		)
	}

	return Plan{Manifest: manifest, Artifacts: artifacts}, nil
}

func PushOCI(ctx context.Context, rootDir, ref string) error {
	if rootDir == "" {
		return errors.New("output dir is required")
	}

	ref = strings.TrimPrefix(ref, "oci://")
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

	store, err := file.New(rootDir)
	if err != nil {
		return fmt.Errorf("open file store %q: %w", rootDir, err)
	}
	defer store.Close() //nolint:errcheck // best effort close

	paths, err := collectArtifactPaths(rootDir)
	if err != nil {
		return err
	}

	descriptors := make([]ocispec.Descriptor, 0, len(paths))
	for _, p := range paths {
		desc, err := store.Add(ctx, p, fileMediaType, p)
		if err != nil {
			return fmt.Errorf("add %q to OCI artifact: %w", p, err)
		}
		descriptors = append(descriptors, desc)
	}

	manifestDesc, err := oras.PackManifest(ctx, store, oras.PackManifestVersion1_1, artifactType, oras.PackManifestOptions{Layers: descriptors})
	if err != nil {
		return fmt.Errorf("pack OCI artifact manifest: %w", err)
	}

	tag := repo.Reference.Reference
	if err := store.Tag(ctx, manifestDesc, tag); err != nil {
		return fmt.Errorf("tag OCI artifact %q: %w", tag, err)
	}

	if _, err := oras.Copy(ctx, store, tag, repo, tag, oras.DefaultCopyOptions); err != nil {
		return fmt.Errorf("push OCI artifact %q: %w", ref, err)
	}

	return nil
}

func writeManifest(rootDir string, manifest Manifest) error {
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		return fmt.Errorf("create output dir %q: %w", rootDir, err)
	}

	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal manifest: %w", err)
	}
	data = append(data, '\n')

	path := filepath.Join(rootDir, ManifestFileName)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write manifest %q: %w", path, err)
	}

	return nil
}

func downloadArtifacts(ctx context.Context, rootDir string, artifacts []Artifact, skipExisting bool) error {
	eg, ctx := errgroup.WithContext(ctx)
	eg.SetLimit(4)

	for _, artifact := range artifacts {
		artifact := artifact
		eg.Go(func() error {
			return downloadArtifact(ctx, rootDir, artifact, skipExisting)
		})
	}

	if err := eg.Wait(); err != nil {
		return err
	}

	for _, artifact := range artifacts {
		if artifact.Name == "kubelet" || artifact.Name == "kubectl" || artifact.Name == "kube-proxy" {
			path := filepath.Join(rootDir, filepath.FromSlash(artifact.Path))
			if err := verifyKubernetesChecksum(path); err != nil {
				return err
			}
		}
	}

	return nil
}

func downloadArtifact(ctx context.Context, rootDir string, artifact Artifact, skipExisting bool) error {
	dest := filepath.Join(rootDir, filepath.FromSlash(artifact.Path))
	if skipExisting {
		if _, err := os.Stat(dest); err == nil {
			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("stat %q: %w", dest, err)
		}
	}

	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("create dir for %q: %w", dest, err)
	}

	tmp := dest + ".tmp"
	if err := downloadToFile(ctx, artifact.URL, tmp); err != nil {
		os.Remove(tmp) //nolint:errcheck // best effort cleanup
		return fmt.Errorf("download %s to %q: %w", artifact.URL, dest, err)
	}

	if err := os.Rename(tmp, dest); err != nil {
		os.Remove(tmp) //nolint:errcheck // best effort cleanup
		return fmt.Errorf("install %q: %w", dest, err)
	}

	return nil
}

func downloadToFile(ctx context.Context, sourceURL, dest string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sourceURL, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	client := &http.Client{Timeout: 10 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close() //nolint:errcheck // best effort close

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("unexpected status %s", resp.Status)
	}

	out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer out.Close() //nolint:errcheck // best effort close

	if _, err := io.Copy(out, resp.Body); err != nil {
		return err
	}

	return nil
}

func verifyKubernetesChecksum(binaryPath string) error {
	checksumPath := binaryPath + ".sha256"
	checksumBytes, err := os.ReadFile(checksumPath)
	if err != nil {
		return fmt.Errorf("read checksum %q: %w", checksumPath, err)
	}

	want := strings.Fields(string(checksumBytes))
	if len(want) == 0 {
		return fmt.Errorf("checksum %q is empty", checksumPath)
	}

	file, err := os.Open(binaryPath)
	if err != nil {
		return fmt.Errorf("open %q: %w", binaryPath, err)
	}
	defer file.Close() //nolint:errcheck // best effort close

	h := sha256.New()
	if _, err := io.Copy(h, file); err != nil {
		return fmt.Errorf("hash %q: %w", binaryPath, err)
	}

	got := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(want[0], got) {
		return fmt.Errorf("checksum mismatch for %q: got %s, want %s", binaryPath, got, want[0])
	}

	return nil
}

func resolveManifest(opts Options) (Manifest, error) {
	manifest := opts.Manifest
	if opts.ManifestPath != "" {
		loaded, err := loadManifest(opts.ManifestPath)
		if err != nil {
			return Manifest{}, err
		}
		manifest = loaded
	}

	return agentartifacts.NormalizeManifest(manifest)
}

func loadManifest(path string) (Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, fmt.Errorf("read manifest %q: %w", path, err)
	}

	var manifest Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return Manifest{}, fmt.Errorf("parse manifest %q: %w", path, err)
	}

	return manifest, nil
}

func collectArtifactPaths(rootDir string) ([]string, error) {
	var paths []string
	if err := filepath.WalkDir(rootDir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}

		rel, err := filepath.Rel(rootDir, path)
		if err != nil {
			return err
		}

		paths = append(paths, filepath.ToSlash(rel))
		return nil
	}); err != nil {
		return nil, fmt.Errorf("collect artifact paths from %q: %w", rootDir, err)
	}

	sort.Strings(paths)
	return paths, nil
}
