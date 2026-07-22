// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package goalstates

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"text/template"

	"github.com/Azure/unbounded/pkg/agent/artifactsource"
	"github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/internal/utilio"
)

// OfflineArtifactManifestFileName is the manifest file name in an offline artifact bundle.
const (
	OfflineArtifactManifestFileName = "manifest.json"
	offlineArtifactCacheReadyFile   = ".ready"
)

var offlineKubernetesBinaries = []string{"kubelet", "kubectl", "kube-proxy"}

// OfflineArtifactVersions records the component versions included in an offline artifact bundle.
type OfflineArtifactVersions struct {
	Kubernetes string `json:"kubernetes"`
	Containerd string `json:"containerd"`
	Runc       string `json:"runc"`
	CNI        string `json:"cni"`
	Crictl     string `json:"crictl"`
}

// OfflineArtifactManifest describes an offline artifact bundle.
type OfflineArtifactManifest struct {
	SchemaVersion   int                     `json:"schemaVersion,omitempty"`
	Versions        OfflineArtifactVersions `json:"versions"`
	ContainerImages []string                `json:"containerImages"`
}

// OfflineTemplateData is the data available when rendering OfflineArtifacts.Source.
type OfflineTemplateData struct {
	KubernetesVersion    string
	KubernetesVersionNoV string
}

type offlineArtifactSource struct {
	root     string
	artifact artifactsource.Source
}

// ResolvedOfflineArtifacts is a validated offline artifact source and manifest.
type ResolvedOfflineArtifacts struct {
	SourceRoot string
	Manifest   OfflineArtifactManifest
	source     artifactsource.Source
}

// ContainerImageArchiveStaging describes host-side staged container image archives.
type ContainerImageArchiveStaging struct {
	// HostDir is the source-specific host directory containing staged archives.
	HostDir string
	// URLs lists archive sources to download into HostDir.
	URLs []string
}

func emptyContainerImageArchiveStaging() *ContainerImageArchiveStaging {
	return &ContainerImageArchiveStaging{HostDir: filepath.Join(ContainerImageArchiveHostSourceDir, "empty")}
}

func resolveOfflineArtifacts(ctx context.Context, cfg *config.AgentConfig, offline *config.AgentOfflineArtifacts) (*ResolvedOfflineArtifacts, error) {
	if offline == nil || strings.TrimSpace(offline.Source) == "" {
		return nil, errors.New("OfflineArtifacts.Source is required")
	}

	if cfg == nil {
		return nil, errors.New("agent config is required")
	}

	clusterVersion := normalizeKubernetesVersion(cfg.Cluster.Version)
	if clusterVersion == "v" {
		return nil, errors.New("Cluster.Version is required when OfflineArtifacts.Source is configured")
	}

	renderedSource, err := RenderOfflineSource(offline.Source, clusterVersion)
	if err != nil {
		return nil, err
	}

	source, err := normalizeOfflineSourceRoot(renderedSource)
	if err != nil {
		return nil, err
	}

	var archiveCache *artifactsource.ArchiveCache
	if source.artifact.Kind() == artifactsource.KindHTTP {
		archiveCache, err = source.artifact.MaterializeArchive(ctx, artifactsource.ArchiveCacheOptions{
			CacheRoot:   OfflineArtifactArchiveHostDir,
			RootMarker:  OfflineArtifactManifestFileName,
			ReadyMarker: offlineArtifactCacheReadyFile,
		})
		if err != nil {
			return nil, fmt.Errorf("materialize HTTPS offline artifact archive: %w", err)
		}

		source, err = normalizeOfflineSourceRoot(archiveCache.Root())
		if err != nil {
			return nil, fmt.Errorf("resolve extracted HTTPS offline artifact cache: %w", err)
		}
	}

	manifest, err := loadOfflineManifest(ctx, source)
	if err != nil {
		return nil, err
	}

	if manifest.Versions.Kubernetes != clusterVersion {
		return nil, fmt.Errorf("offline artifacts Kubernetes version %q does not match Cluster.Version %q", manifest.Versions.Kubernetes, clusterVersion)
	}

	if err := validateRuntimeVersionConflicts(cfg, manifest); err != nil {
		return nil, err
	}

	if err := verifyOfflineFiles(ctx, source, manifest); err != nil {
		return nil, err
	}

	if archiveCache != nil {
		if err := archiveCache.MarkReady(); err != nil {
			return nil, err
		}
	}

	return &ResolvedOfflineArtifacts{SourceRoot: source.root, Manifest: manifest, source: source.artifact}, nil
}

// RenderOfflineSource renders an OfflineArtifacts.Source template for the given
// Kubernetes version.
func RenderOfflineSource(sourceTemplate, kubernetesVersion string) (string, error) {
	data := OfflineTemplateData{
		KubernetesVersion:    kubernetesVersion,
		KubernetesVersionNoV: stripLeadingV(kubernetesVersion),
	}

	tmpl, err := template.New("offline-artifacts-source").Option("missingkey=error").Parse(sourceTemplate)
	if err != nil {
		return "", fmt.Errorf("parse OfflineArtifacts.Source template: %w", err)
	}

	var out bytes.Buffer
	if err := tmpl.Execute(&out, data); err != nil {
		return "", fmt.Errorf("render OfflineArtifacts.Source template: %w", err)
	}

	return out.String(), nil
}

func normalizeOfflineSourceRoot(source string) (offlineArtifactSource, error) {
	source = strings.TrimSpace(source)
	if source == "" {
		return offlineArtifactSource{}, errors.New("offline artifact source is empty")
	}

	typedSource, err := artifactsource.ParseRoot(source)
	if err != nil {
		return offlineArtifactSource{}, err
	}

	switch typedSource.Kind() {
	case artifactsource.KindLocal:
		return normalizeLocalOfflineSource(typedSource)
	case artifactsource.KindHTTP:
		return normalizeHTTPSOfflineSource(typedSource)
	case artifactsource.KindOCI:
		return normalizeOCIOfflineSource(typedSource)
	default:
		return offlineArtifactSource{}, fmt.Errorf("unsupported OfflineArtifacts.Source kind %d", typedSource.Kind())
	}
}

func normalizeLocalOfflineSource(typedSource artifactsource.Source) (offlineArtifactSource, error) {
	localPath, ok := typedSource.LocalPath()
	if !ok {
		return offlineArtifactSource{}, fmt.Errorf("offline artifact source %q is not local", typedSource.String())
	}

	return offlineArtifactSource{
		root:     localPath,
		artifact: typedSource,
	}, nil
}

func normalizeHTTPSOfflineSource(typedSource artifactsource.Source) (offlineArtifactSource, error) {
	parsed, err := url.Parse(typedSource.String())
	if err != nil {
		return offlineArtifactSource{}, fmt.Errorf("parse HTTPS offline artifact source: %w", utilio.RedactHTTPError(err))
	}

	if parsed.Scheme != "https" {
		return offlineArtifactSource{}, fmt.Errorf("unsupported OfflineArtifacts.Source scheme %q", parsed.Scheme)
	}

	if parsed.Host == "" || strings.Trim(parsed.Path, "/") == "" {
		return offlineArtifactSource{}, errors.New("HTTPS offline artifact source must include a host and archive path")
	}

	if parsed.User != nil || parsed.Fragment != "" {
		return offlineArtifactSource{}, errors.New("HTTPS offline artifact source must not include user info or a fragment")
	}

	parsed.Path = strings.TrimRight(parsed.Path, "/")
	parsed.RawPath = strings.TrimRight(parsed.RawPath, "/")
	root := parsed.String()

	typedSource, err = artifactsource.ParseRoot(root)
	if err != nil {
		return offlineArtifactSource{}, fmt.Errorf("parse HTTPS offline artifact archive: %w", err)
	}

	return offlineArtifactSource{
		root:     root,
		artifact: typedSource,
	}, nil
}

func normalizeOCIOfflineSource(typedRoot artifactsource.Source) (offlineArtifactSource, error) {
	parsed, err := url.Parse(typedRoot.String())
	if err != nil {
		return offlineArtifactSource{}, fmt.Errorf("parse OCI offline artifact source: %w", err)
	}

	if parsed.Host == "" || strings.Trim(parsed.Path, "/") == "" {
		return offlineArtifactSource{}, errors.New("OCI offline artifact source must include registry and repository")
	}

	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return offlineArtifactSource{}, errors.New("OCI offline artifact source must not include user info, query parameters, or a fragment")
	}

	parsed.Path = strings.TrimRight(parsed.Path, "/")
	parsed.RawPath = strings.TrimRight(parsed.RawPath, "/")
	root := parsed.String()

	typedRoot, err = artifactsource.ParseRoot(root)
	if err != nil {
		return offlineArtifactSource{}, fmt.Errorf("parse OCI offline artifact root: %w", err)
	}

	return offlineArtifactSource{
		root:     root,
		artifact: typedRoot,
	}, nil
}

func loadOfflineManifest(ctx context.Context, source offlineArtifactSource) (OfflineArtifactManifest, error) {
	manifestSource, err := source.artifact.Artifact(OfflineArtifactManifestFileName)
	if err != nil {
		return OfflineArtifactManifest{}, fmt.Errorf("resolve offline artifact manifest source: %w", err)
	}

	data, err := manifestSource.ReadAll(ctx)
	if err != nil {
		return OfflineArtifactManifest{}, fmt.Errorf("read offline artifact manifest from %q: %w", source.root, err)
	}

	var manifest OfflineArtifactManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return OfflineArtifactManifest{}, fmt.Errorf("parse offline artifact manifest from %q: %w", source.root, err)
	}

	manifest, err = normalizeOfflineManifest(manifest)
	if err != nil {
		return OfflineArtifactManifest{}, fmt.Errorf("validate offline artifact manifest from %q: %w", source.root, err)
	}

	return manifest, nil
}

func validateRuntimeVersionConflicts(cfg *config.AgentConfig, manifest OfflineArtifactManifest) error {
	var errs []error
	if cfg.CRI.Containerd.Version != "" && stripLeadingV(cfg.CRI.Containerd.Version) != manifest.Versions.Containerd {
		errs = append(errs, fmt.Errorf("CRI.Containerd.Version %q conflicts with offline manifest containerd version %q", cfg.CRI.Containerd.Version, manifest.Versions.Containerd))
	}

	if cfg.CRI.Runc.Version != "" && stripLeadingV(cfg.CRI.Runc.Version) != manifest.Versions.Runc {
		errs = append(errs, fmt.Errorf("CRI.Runc.Version %q conflicts with offline manifest runc version %q", cfg.CRI.Runc.Version, manifest.Versions.Runc))
	}

	if cfg.CNI.PluginVersion != "" && stripLeadingV(cfg.CNI.PluginVersion) != manifest.Versions.CNI {
		errs = append(errs, fmt.Errorf("CNI.PluginVersion %q conflicts with offline manifest cni version %q", cfg.CNI.PluginVersion, manifest.Versions.CNI))
	}

	return errors.Join(errs...)
}

func verifyOfflineFiles(ctx context.Context, source offlineArtifactSource, manifest OfflineArtifactManifest) error {
	paths := offlineArtifactPaths(manifest, runtime.GOARCH)
	if source.artifact.Kind() == artifactsource.KindOCI {
		return verifyOCIArtifacts(ctx, source.artifact, paths)
	}

	if source.artifact.Kind() != artifactsource.KindLocal {
		return fmt.Errorf("unsupported resolved offline artifact source kind %d", source.artifact.Kind())
	}

	var errs []error

	for _, path := range paths {
		fullPath := filepath.Join(source.root, filepath.FromSlash(path))

		info, err := os.Stat(fullPath)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("required offline artifact %q is missing: %w", path, err))
		case info.IsDir():
			errs = append(errs, fmt.Errorf("required offline artifact %q is a directory", path))
		}
	}

	return errors.Join(errs...)
}

func offlineArtifactPaths(manifest OfflineArtifactManifest, arch string) []string {
	paths := []string{OfflineArtifactManifestFileName}

	for _, binary := range offlineKubernetesBinaries {
		path := offlineKubernetesArtifactPath(manifest.Versions.Kubernetes, arch, binary)
		paths = append(paths, path, path+".sha256")
	}

	paths = append(paths,
		offlineContainerdArtifactPath(manifest.Versions.Containerd, arch),
		offlineRuncArtifactPath(manifest.Versions.Runc, arch),
		offlineCNIArtifactPath(manifest.Versions.CNI, arch),
		offlineCrictlArtifactPath(manifest.Versions.Crictl, "linux", arch),
	)

	for _, imageTag := range manifest.ContainerImages {
		path := offlineContainerImageArchivePath(arch, imageTag)
		paths = append(paths, path, path+".sha256")
	}

	return paths
}

func verifyOCIArtifacts(ctx context.Context, source artifactsource.Source, paths []string) error {
	titles, err := source.OCIArtifactTitles(ctx)
	if err != nil {
		return err
	}

	var errs []error

	for _, path := range paths {
		if _, ok := titles[path]; !ok {
			errs = append(errs, fmt.Errorf("required offline artifact %q is missing", path))
		}
	}

	return errors.Join(errs...)
}

// ResolveDownloadOverridesWithOfflineArtifacts resolves AgentConfig.OfflineArtifacts
// and returns download overrides plus container image archive staging metadata
// that point at the offline artifact source. When OfflineArtifacts is not
// configured, the input downloads are returned unchanged and staging points at
// the host-side empty archive directory.
func ResolveDownloadOverridesWithOfflineArtifacts(ctx context.Context, cfg *config.AgentConfig, downloads *DownloadOverrides) (*DownloadOverrides, *ContainerImageArchiveStaging, error) {
	if cfg == nil || cfg.OfflineArtifacts == nil || strings.TrimSpace(cfg.OfflineArtifacts.Source) == "" {
		return downloads, emptyContainerImageArchiveStaging(), nil
	}

	resolved, err := resolveOfflineArtifacts(ctx, cfg, cfg.OfflineArtifacts)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve bootstrap artifact sources: %w", err)
	}

	staging := &ContainerImageArchiveStaging{
		HostDir: containerImageArchiveHostDir(resolved.SourceRoot),
		URLs:    containerImageArchiveURLsFromOfflineArtifacts(resolved, runtime.GOARCH),
	}

	return downloadOverridesFromOfflineArtifacts(resolved), staging, nil
}

func downloadOverridesFromOfflineArtifacts(offlineArtifacts *ResolvedOfflineArtifacts) *DownloadOverrides {
	manifest := offlineArtifacts.Manifest
	rootURL := offlineArtifactURLRoot(offlineArtifacts.SourceRoot, offlineArtifacts.source.Kind())
	separator := offlineArtifactURLSeparator(offlineArtifacts.source.Kind())

	overrides := &DownloadOverrides{
		Kubernetes: &DownloadSource{
			URL:     rootURL + separator + "kubernetes/v%s/bin/linux/%s/%s",
			Version: stripLeadingV(manifest.Versions.Kubernetes),
		},
		Containerd: &DownloadSource{
			URL:     rootURL + separator + "containerd/v%s/containerd-%s-linux-%s.tar.gz",
			Version: manifest.Versions.Containerd,
		},
		Runc: &DownloadSource{
			URL:     rootURL + separator + "runc/v%s/runc.%s",
			Version: manifest.Versions.Runc,
		},
		CNI: &DownloadSource{
			URL:     rootURL + separator + "cni/v%s/cni-plugins-linux-%s-v%s.tgz",
			Version: manifest.Versions.CNI,
		},
		Crictl: &DownloadSource{
			URL:     rootURL + separator + "crictl/v%s/crictl-v%s-%s-%s.tar.gz",
			Version: manifest.Versions.Crictl,
		},
	}

	return overrides
}

func containerImageArchiveURLsFromOfflineArtifacts(offlineArtifacts *ResolvedOfflineArtifacts, arch string) []string {
	if offlineArtifacts == nil {
		return []string{}
	}

	rootURL := offlineArtifactURLRoot(offlineArtifacts.SourceRoot, offlineArtifacts.source.Kind())
	separator := offlineArtifactURLSeparator(offlineArtifacts.source.Kind())

	urls := make([]string, 0, len(offlineArtifacts.Manifest.ContainerImages))
	for _, imageTag := range offlineArtifacts.Manifest.ContainerImages {
		urls = append(urls, rootURL+separator+offlineContainerImageArchivePath(arch, imageTag))
	}

	return urls
}

func containerImageArchiveHostDir(sourceRoot string) string {
	return filepath.Join(ContainerImageArchiveHostSourceDir, artifactsource.CacheKey(sourceRoot))
}

func offlineArtifactURLRoot(sourceRoot string, sourceKind artifactsource.Kind) string {
	if sourceKind == artifactsource.KindOCI {
		return sourceRoot
	}

	return fileURL(sourceRoot)
}

func offlineArtifactURLSeparator(sourceKind artifactsource.Kind) string {
	if sourceKind == artifactsource.KindOCI {
		return "#"
	}

	return "/"
}

func fileURL(path string) string {
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(path)}).String()
}

func offlineKubernetesArtifactPath(version, arch, binary string) string {
	return fmt.Sprintf("kubernetes/%s/bin/linux/%s/%s", normalizeKubernetesVersion(version), arch, binary)
}

func offlineContainerdArtifactPath(version, arch string) string {
	version = stripLeadingV(version)
	return fmt.Sprintf("containerd/v%s/containerd-%s-linux-%s.tar.gz", version, version, arch)
}

func offlineRuncArtifactPath(version, arch string) string {
	return fmt.Sprintf("runc/v%s/runc.%s", stripLeadingV(version), arch)
}

func offlineCNIArtifactPath(version, arch string) string {
	version = stripLeadingV(version)
	return fmt.Sprintf("cni/v%s/cni-plugins-linux-%s-v%s.tgz", version, arch, version)
}

func offlineCrictlArtifactPath(version, hostOS, arch string) string {
	version = stripLeadingV(version)
	return fmt.Sprintf("crictl/v%s/crictl-v%s-%s-%s.tar.gz", version, version, hostOS, arch)
}

func offlineContainerImageArchivePath(arch, imageTag string) string {
	imageTag = strings.TrimSpace(imageTag)
	name := strings.NewReplacer(
		"/", "_",
		":", "_",
		"@", "_",
	).Replace(imageTag)
	digest := sha256.Sum256([]byte(imageTag))

	return fmt.Sprintf("container-images/%s/%s-%x.tar", arch, name, digest[:6])
}

func normalizeOfflineManifest(manifest OfflineArtifactManifest) (OfflineArtifactManifest, error) {
	if manifest.SchemaVersion == 0 {
		manifest.SchemaVersion = 1
	}

	if manifest.SchemaVersion != 1 {
		return OfflineArtifactManifest{}, fmt.Errorf("unsupported manifest schemaVersion %d", manifest.SchemaVersion)
	}

	manifest.Versions.Kubernetes = normalizeKubernetesVersion(manifest.Versions.Kubernetes)
	manifest.Versions.Containerd = stripLeadingV(manifest.Versions.Containerd)
	manifest.Versions.Runc = stripLeadingV(manifest.Versions.Runc)
	manifest.Versions.CNI = stripLeadingV(manifest.Versions.CNI)
	manifest.Versions.Crictl = stripLeadingV(manifest.Versions.Crictl)
	manifest.ContainerImages = normalizeContainerImages(manifest.ContainerImages)

	missing := make([]string, 0, 5)
	if manifest.Versions.Kubernetes == "v" {
		missing = append(missing, "versions.kubernetes")
	}

	if manifest.Versions.Containerd == "" {
		missing = append(missing, "versions.containerd")
	}

	if manifest.Versions.Runc == "" {
		missing = append(missing, "versions.runc")
	}

	if manifest.Versions.CNI == "" {
		missing = append(missing, "versions.cni")
	}

	if manifest.Versions.Crictl == "" {
		missing = append(missing, "versions.crictl")
	}

	if len(missing) > 0 {
		return OfflineArtifactManifest{}, fmt.Errorf("manifest is missing required fields: %s", strings.Join(missing, ", "))
	}

	return manifest, nil
}

func normalizeKubernetesVersion(version string) string {
	version = strings.TrimSpace(version)
	if strings.HasPrefix(version, "v") {
		return version
	}

	return "v" + version
}

func stripLeadingV(version string) string {
	return strings.TrimPrefix(strings.TrimSpace(version), "v")
}

func normalizeContainerImages(images []string) []string {
	seen := map[string]struct{}{}

	out := make([]string, 0, len(images))
	for _, image := range images {
		image = strings.TrimSpace(image)
		if image == "" {
			continue
		}

		if _, ok := seen[image]; ok {
			continue
		}

		seen[image] = struct{}{}
		out = append(out, image)
	}

	sort.Strings(out)

	return out
}
