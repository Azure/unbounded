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
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/google/renameio/v2"
	specs "github.com/opencontainers/image-spec/specs-go"
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
	"github.com/Azure/unbounded/pkg/agent/artifactsource"
	"github.com/Azure/unbounded/pkg/agent/bootstrapartifacts"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

const (
	ManifestFileName = bootstrapartifacts.ManifestFileName

	artifactType  = "application/vnd.unbounded.agent.bootstrap-artifacts.v1"
	fileMediaType = "application/octet-stream"
)

type Options struct {
	OutputDir         string
	StagingDir        string
	ArchivePath       string
	OCIRef            string
	ManifestPath      string
	Manifest          bootstrapartifacts.Manifest
	KubernetesVersion string

	Architectures []string
}

type Artifact struct {
	Name             string
	URL              string
	Path             string
	GenerateChecksum bool
}

type ContainerImageArchive struct {
	ImageTag string
	Arch     string
	Path     string
}

type Plan struct {
	Manifest        bootstrapartifacts.Manifest
	Artifacts       []Artifact
	ContainerImages []ContainerImageArchive
}

func Build(ctx context.Context, log *slog.Logger, opts Options) error {
	plan, err := NewPlan(opts)
	if err != nil {
		return err
	}

	stagingDir := opts.StagingDir
	if stagingDir == "" {
		var cleanup func()

		stagingDir, cleanup, err = NewStagingDir()
		if err != nil {
			return err
		}
		defer cleanup()
	}

	acquireGroup, acquireCtx := errgroup.WithContext(ctx)
	acquireGroup.Go(func() error {
		return downloadArtifacts(acquireCtx, log, stagingDir, plan.Artifacts)
	})
	acquireGroup.Go(func() error {
		return exportContainerImages(acquireCtx, log, stagingDir, plan.ContainerImages)
	})

	if err := acquireGroup.Wait(); err != nil {
		return err
	}

	materializeGroup := &errgroup.Group{}
	materializeGroup.Go(func() error {
		return materializeArtifacts(stagingDir, opts.OutputDir, plan.Artifacts)
	})
	materializeGroup.Go(func() error {
		return materializeContainerImages(stagingDir, opts.OutputDir, plan.ContainerImages)
	})

	if err := materializeGroup.Wait(); err != nil {
		return err
	}

	if err := writeManifest(opts.OutputDir, plan.Manifest); err != nil {
		return err
	}

	if opts.OCIRef != "" || opts.ArchivePath != "" {
		if err := ValidateBundle(log, opts.OutputDir); err != nil {
			return err
		}
	}

	if opts.ArchivePath != "" {
		if err := WriteBundleArchive(opts.OutputDir, opts.ArchivePath); err != nil {
			return err
		}
	}

	if opts.OCIRef != "" {
		if err := PushOCI(ctx, log, opts.OutputDir, opts.OCIRef); err != nil {
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

	containerImages := make([]ContainerImageArchive, 0, len(arches)*len(manifest.ContainerImages))
	for _, arch := range arches {
		for _, binary := range bootstrapartifacts.KubernetesBinaries {
			path := bootstrapartifacts.KubernetesArtifactPath(manifest.Versions.Kubernetes, arch, binary)
			url := agentartifacts.KubernetesBinary(nil, manifest.Versions.Kubernetes, arch, binary)
			artifacts = append(artifacts, Artifact{Name: binary, URL: url, Path: path})
			artifacts = append(artifacts, Artifact{Name: binary + ".sha256", URL: url + ".sha256", Path: path + ".sha256"})
		}

		for _, imageTag := range manifest.ContainerImages {
			containerImages = append(containerImages, ContainerImageArchive{
				ImageTag: imageTag,
				Arch:     arch,
				Path:     bootstrapartifacts.ContainerImageArchivePath(arch, imageTag),
			})
		}

		artifacts = append(artifacts,
			Artifact{
				Name:             "containerd",
				URL:              agentartifacts.ContainerdArchive(nil, manifest.Versions.Containerd, arch),
				Path:             bootstrapartifacts.ContainerdArtifactPath(manifest.Versions.Containerd, arch),
				GenerateChecksum: true,
			},
			Artifact{
				Name:             "runc",
				URL:              agentartifacts.RuncBinary(nil, manifest.Versions.Runc, arch),
				Path:             bootstrapartifacts.RuncArtifactPath(manifest.Versions.Runc, arch),
				GenerateChecksum: true,
			},
			Artifact{
				Name:             "cni",
				URL:              agentartifacts.CNIPluginsArchive(nil, manifest.Versions.CNI, arch),
				Path:             bootstrapartifacts.CNIArtifactPath(manifest.Versions.CNI, arch),
				GenerateChecksum: true,
			},
			Artifact{
				Name:             "crictl",
				URL:              agentartifacts.CrictlArchive(nil, manifest.Versions.Crictl, "linux", arch),
				Path:             bootstrapartifacts.CrictlArtifactPath(manifest.Versions.Crictl, "linux", arch),
				GenerateChecksum: true,
			},
		)
	}

	return Plan{Manifest: manifest, Artifacts: artifacts, ContainerImages: containerImages}, nil
}

func PushOCI(ctx context.Context, log *slog.Logger, rootDir, ref string) error {
	if rootDir == "" {
		return errors.New("output dir is required")
	}

	ref = strings.TrimPrefix(ref, "oci://")
	log.Info("pushing offline artifact bundle", slog.String("oci_ref", "oci://"+ref))

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

	manifest, err := loadManifest(filepath.Join(rootDir, ManifestFileName))
	if err != nil {
		return err
	}

	manifest, err = bootstrapartifacts.NormalizeManifest(manifest)
	if err != nil {
		return err
	}

	architectures, err := detectArchitectures(rootDir, manifest.Versions.Kubernetes)
	if err != nil {
		return err
	}

	descriptorsByPath := map[string]ocispec.Descriptor{}

	platformManifests := make([]ocispec.Descriptor, 0, len(architectures))
	for _, arch := range architectures {
		desc, err := packPlatformManifest(ctx, store, manifest, arch, descriptorsByPath)
		if err != nil {
			return err
		}

		platformManifests = append(platformManifests, desc)
	}

	index := ocispec.Index{
		Versioned:    specs.Versioned{SchemaVersion: 2},
		MediaType:    ocispec.MediaTypeImageIndex,
		ArtifactType: artifactType,
		Manifests:    platformManifests,
	}

	indexBytes, err := json.Marshal(index)
	if err != nil {
		return fmt.Errorf("marshal OCI artifact index: %w", err)
	}

	indexDesc, err := oras.PushBytes(ctx, store, ocispec.MediaTypeImageIndex, indexBytes)
	if err != nil {
		return fmt.Errorf("push OCI artifact index: %w", err)
	}

	indexDesc.ArtifactType = artifactType

	tag := repo.Reference.Reference
	if err := store.Tag(ctx, indexDesc, tag); err != nil {
		return fmt.Errorf("tag OCI artifact %q: %w", tag, err)
	}

	desc, err := oras.Copy(ctx, store, tag, repo, tag, oras.DefaultCopyOptions)
	if err != nil {
		return fmt.Errorf("push OCI artifact %q: %w", ref, err)
	}

	log.Info("pushed offline artifact bundle", slog.String("oci_ref", "oci://"+ref), slog.String("digest", desc.Digest.String()))

	return nil
}

func packPlatformManifest(ctx context.Context, store *file.Store, manifest bootstrapartifacts.Manifest, arch string, descriptorsByPath map[string]ocispec.Descriptor) (ocispec.Descriptor, error) {
	plan, err := NewPlan(Options{
		OutputDir:     ".",
		Manifest:      manifest,
		Architectures: []string{arch},
	})
	if err != nil {
		return ocispec.Descriptor{}, err
	}

	paths := expectedBundlePaths(plan)

	descriptors := make([]ocispec.Descriptor, 0, len(paths))
	for _, path := range paths {
		desc, ok := descriptorsByPath[path]
		if !ok {
			var err error

			desc, err = store.Add(ctx, path, fileMediaType, path)
			if err != nil {
				return ocispec.Descriptor{}, fmt.Errorf("add %q to OCI artifact: %w", path, err)
			}

			descriptorsByPath[path] = desc
		}

		descriptors = append(descriptors, desc)
	}

	manifestDesc, err := oras.PackManifest(ctx, store, oras.PackManifestVersion1_1, artifactType, oras.PackManifestOptions{Layers: descriptors})
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("pack %s OCI artifact manifest: %w", arch, err)
	}

	manifestDesc.ArtifactType = artifactType
	manifestDesc.Platform = &ocispec.Platform{OS: "linux", Architecture: arch}

	return manifestDesc, nil
}

func NewStagingDir() (string, func(), error) {
	base := filepath.Join("tmp", "agent-artifacts-builder-stage")
	if err := os.MkdirAll(base, 0o755); err != nil {
		return "", nil, fmt.Errorf("create staging base dir %q: %w", base, err)
	}

	dir, err := os.MkdirTemp(base, "")
	if err != nil {
		return "", nil, fmt.Errorf("create staging dir: %w", err)
	}

	cleanup := func() {
		os.RemoveAll(dir) //nolint:errcheck // best effort cleanup
	}

	return dir, cleanup, nil
}

func materializeArtifacts(stagingDir, outputDir string, artifacts []Artifact) error {
	for _, artifact := range artifacts {
		if err := materializeArtifact(stagingDir, outputDir, artifact.Path); err != nil {
			return err
		}

		if artifact.GenerateChecksum {
			if err := materializeArtifact(stagingDir, outputDir, artifact.Path+".sha256"); err != nil {
				return err
			}
		}
	}

	return nil
}

func materializeContainerImages(stagingDir, outputDir string, images []ContainerImageArchive) error {
	for _, image := range images {
		if err := materializeArtifact(stagingDir, outputDir, image.Path); err != nil {
			return err
		}

		if err := materializeArtifact(stagingDir, outputDir, image.Path+".sha256"); err != nil {
			return err
		}
	}

	return nil
}

func materializeArtifact(stagingDir, outputDir, path string) error {
	source := filepath.Join(stagingDir, filepath.FromSlash(path))
	dest := filepath.Join(outputDir, filepath.FromSlash(path))

	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("create dir for %q: %w", dest, err)
	}

	if err := os.Link(source, dest); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrExist) {
		if copyErr := copyFileAtomically(source, dest); copyErr != nil {
			return fmt.Errorf("materialize %q: link failed: %w; copy failed: %w", path, err, copyErr)
		}

		return nil
	}

	return copyFileAtomically(source, dest)
}

func copyFileAtomically(source, dest string) error {
	info, err := os.Stat(source)
	if err != nil {
		return fmt.Errorf("stat %q: %w", source, err)
	}

	data, err := os.ReadFile(source)
	if err != nil {
		return fmt.Errorf("read %q: %w", source, err)
	}

	if err := renameio.WriteFile(dest, data, info.Mode().Perm()); err != nil {
		return fmt.Errorf("install %q: %w", dest, err)
	}

	return nil
}

func writeManifest(rootDir string, manifest bootstrapartifacts.Manifest) error {
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		return fmt.Errorf("create output dir %q: %w", rootDir, err)
	}

	// Omit schemaVersion for v1 manifests. The agent treats a missing schema
	// version as v1, and this keeps bundles stable until a breaking manifest
	// schema is introduced.
	if manifest.SchemaVersion == 1 {
		manifest.SchemaVersion = 0
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

func downloadArtifacts(ctx context.Context, log *slog.Logger, rootDir string, artifacts []Artifact) error {
	eg, ctx := errgroup.WithContext(ctx)
	eg.SetLimit(8)

	for _, artifact := range artifacts {
		eg.Go(func() error {
			return downloadArtifact(ctx, log, rootDir, artifact)
		})
	}

	if err := eg.Wait(); err != nil {
		return err
	}

	for _, artifact := range artifacts {
		path := filepath.Join(rootDir, filepath.FromSlash(artifact.Path))
		if artifact.GenerateChecksum {
			if err := writeGeneratedChecksum(path); err != nil {
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

func downloadArtifact(ctx context.Context, log *slog.Logger, rootDir string, artifact Artifact) error {
	dest := filepath.Join(rootDir, filepath.FromSlash(artifact.Path))
	if _, err := os.Stat(dest); err == nil {
		log.Info("skipping existing artifact", slog.String("artifact", artifact.Path))
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat %q: %w", dest, err)
	}

	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("create dir for %q: %w", dest, err)
	}

	log.Info("downloading artifact", slog.String("artifact", artifact.Path), slog.String("source", artifact.URL))

	if err := downloadToFile(ctx, artifact.URL, dest); err != nil {
		return fmt.Errorf("download %s to %q: %w", artifact.URL, dest, err)
	}

	log.Info("downloaded artifact", slog.String("artifact", artifact.Path))

	return nil
}

func downloadToFile(ctx context.Context, sourceURL, dest string) (err error) {
	source, err := artifactsource.Parse(sourceURL)
	if err != nil {
		return err
	}

	return source.DownloadToLocalFile(ctx, dest, 0o644)
}

func writeGeneratedChecksum(path string) error {
	checksum, err := sha256File(path)
	if err != nil {
		return err
	}

	checksumPath := path + ".sha256"

	data := fmt.Sprintf("%s  %s\n", checksum, filepath.Base(path))
	if err := renameio.WriteFile(checksumPath, []byte(data), 0o644); err != nil {
		return fmt.Errorf("write checksum %q: %w", checksumPath, err)
	}

	return nil
}

func verifyChecksum(binaryPath string) error {
	checksumPath := binaryPath + ".sha256"

	checksumBytes, err := os.ReadFile(checksumPath)
	if err != nil {
		return fmt.Errorf("read checksum %q: %w", checksumPath, err)
	}

	want := strings.Fields(string(checksumBytes))
	if len(want) == 0 {
		return fmt.Errorf("checksum %q is empty", checksumPath)
	}

	got, err := sha256File(binaryPath)
	if err != nil {
		return err
	}

	if !strings.EqualFold(want[0], got) {
		return fmt.Errorf("checksum mismatch for %q: got %s, want %s", binaryPath, got, want[0])
	}

	return nil
}

func sha256File(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open %q: %w", path, err)
	}
	defer file.Close() //nolint:errcheck // read-only close

	h := sha256.New()
	if _, err := io.Copy(h, file); err != nil {
		return "", fmt.Errorf("hash %q: %w", path, err)
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}

func resolveManifest(opts Options) (bootstrapartifacts.Manifest, error) {
	if opts.ManifestPath != "" {
		manifest, err := loadManifest(opts.ManifestPath)
		if err != nil {
			return bootstrapartifacts.Manifest{}, err
		}

		return bootstrapartifacts.NormalizeManifest(manifest)
	}

	if opts.KubernetesVersion != "" {
		manifest, err := defaultManifest(opts.KubernetesVersion)
		if err != nil {
			return bootstrapartifacts.Manifest{}, err
		}

		return manifest, nil
	}

	return bootstrapartifacts.NormalizeManifest(opts.Manifest)
}

func defaultManifest(kubernetesVersion string) (bootstrapartifacts.Manifest, error) {
	crictlVersion, err := agentartifacts.CrictlVersionForKubernetesVersion(kubernetesVersion)
	if err != nil {
		return bootstrapartifacts.Manifest{}, err
	}

	return bootstrapartifacts.NormalizeManifest(bootstrapartifacts.Manifest{
		Versions: bootstrapartifacts.Versions{
			Kubernetes: kubernetesVersion,
			Containerd: goalstates.ContainerdVersion,
			Runc:       goalstates.RunCVersion,
			CNI:        goalstates.CNIPluginVersion,
			Crictl:     crictlVersion,
		},
		ContainerImages: agentartifacts.DefaultContainerImages(kubernetesVersion),
	})
}

func loadManifest(path string) (bootstrapartifacts.Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return bootstrapartifacts.Manifest{}, fmt.Errorf("read manifest %q: %w", path, err)
	}

	var manifest bootstrapartifacts.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return bootstrapartifacts.Manifest{}, fmt.Errorf("parse manifest %q: %w", path, err)
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
