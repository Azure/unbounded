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
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"text/template"

	"github.com/Azure/unbounded/pkg/agent/artifactsource"
	"github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/internal/ociartifact"
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

// ResolvedOfflineArtifacts is a validated offline artifact source and manifest.
type ResolvedOfflineArtifacts struct {
	SourceRoot string
	Manifest   OfflineArtifactManifest
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

	sourceRoot, err := normalizeOfflineSourceRoot(renderedSource)
	if err != nil {
		return nil, err
	}

	httpsArchive := strings.HasPrefix(sourceRoot, "https://")
	if httpsArchive {
		sourceRoot, err = materializeHTTPSOfflineArchive(ctx, sourceRoot)
		if err != nil {
			return nil, err
		}
	}

	manifest, err := loadOfflineManifest(sourceRoot)
	if err != nil {
		return nil, err
	}

	if manifest.Versions.Kubernetes != clusterVersion {
		return nil, fmt.Errorf("offline artifacts Kubernetes version %q does not match Cluster.Version %q", manifest.Versions.Kubernetes, clusterVersion)
	}

	if err := validateRuntimeVersionConflicts(cfg, manifest); err != nil {
		return nil, err
	}

	if err := verifyOfflineFiles(sourceRoot, manifest); err != nil {
		return nil, err
	}

	if httpsArchive {
		if err := markOfflineArchiveCacheReady(sourceRoot); err != nil {
			return nil, err
		}
	}

	return &ResolvedOfflineArtifacts{SourceRoot: sourceRoot, Manifest: manifest}, nil
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

func normalizeOfflineSourceRoot(source string) (string, error) {
	source = strings.TrimSpace(source)
	if source == "" {
		return "", errors.New("offline artifact source is empty")
	}

	if strings.HasPrefix(source, "oci://") {
		u, err := url.Parse(source)
		if err != nil {
			return "", fmt.Errorf("parse OCI offline artifact source %q: %w", source, err)
		}

		if u.Host == "" || strings.Trim(u.Path, "/") == "" {
			return "", fmt.Errorf("OCI offline artifact source must include registry and repository: %q", source)
		}

		if u.Fragment != "" {
			return "", fmt.Errorf("OCI offline artifact source must not include a fragment: %q", source)
		}

		return strings.TrimRight(source, "/"), nil
	}

	if strings.HasPrefix(source, "https://") {
		u, err := url.Parse(source)
		if err != nil {
			return "", fmt.Errorf("parse HTTPS offline artifact source: %w", utilio.RedactHTTPError(err))
		}

		if u.Host == "" || strings.Trim(u.Path, "/") == "" {
			return "", errors.New("HTTPS offline artifact source must include a host and archive path")
		}

		if u.User != nil || u.Fragment != "" {
			return "", errors.New("HTTPS offline artifact source must not include user info or a fragment")
		}

		u.Path = strings.TrimRight(u.Path, "/")
		u.RawPath = strings.TrimRight(u.RawPath, "/")

		return u.String(), nil
	}

	if strings.HasPrefix(source, "file://") {
		u, err := url.Parse(source)
		if err != nil {
			return "", fmt.Errorf("parse file offline artifact source %q: %w", source, err)
		}

		if u.Host != "" && u.Host != "localhost" {
			return "", fmt.Errorf("file offline artifact source must not include host %q", u.Host)
		}

		if u.Path == "" || !filepath.IsAbs(u.Path) {
			return "", fmt.Errorf("file offline artifact source must use an absolute path: %q", source)
		}

		return filepath.Clean(u.Path), nil
	}

	if strings.Contains(source, "://") {
		return "", fmt.Errorf("unsupported OfflineArtifacts.Source scheme in %q", source)
	}

	if !filepath.IsAbs(source) {
		return "", fmt.Errorf("offline artifact source must be an absolute path: %q", source)
	}

	return filepath.Clean(source), nil
}

func materializeHTTPSOfflineArchive(ctx context.Context, archiveURL string) (string, error) {
	source, err := artifactsource.Parse(archiveURL)
	if err != nil {
		return "", fmt.Errorf("parse HTTPS offline artifact archive: %w", err)
	}

	return materializeOfflineArchive(ctx, source, OfflineArtifactArchiveHostDir, archiveURL)
}

func materializeOfflineArchive(ctx context.Context, source artifactsource.Source, cacheRoot, sourceID string) (string, error) {
	cacheDir := filepath.Join(cacheRoot, containerImageArchiveSourceKey(sourceID))
	if isOfflineArchiveCacheReady(cacheDir) {
		return cacheDir, nil
	}

	if err := os.RemoveAll(cacheDir); err != nil {
		return "", fmt.Errorf("remove incomplete HTTPS offline artifact cache: %w", err)
	}

	if err := os.MkdirAll(cacheRoot, 0o750); err != nil {
		return "", fmt.Errorf("create HTTPS offline artifact cache: %w", err)
	}

	tempDir, err := os.MkdirTemp(cacheRoot, ".extract-")
	if err != nil {
		return "", fmt.Errorf("create HTTPS offline artifact extraction directory: %w", err)
	}
	defer os.RemoveAll(tempDir) //nolint:errcheck // best effort cleanup

	if err := source.ExtractTar(ctx, tempDir); err != nil {
		return "", fmt.Errorf("download and extract HTTPS offline artifact archive: %w", err)
	}

	bundleRoot, err := findOfflineArchiveBundleRoot(tempDir)
	if err != nil {
		return "", err
	}

	if _, err := os.Lstat(filepath.Join(bundleRoot, offlineArtifactCacheReadyFile)); err == nil {
		return "", fmt.Errorf("HTTPS offline artifact archive contains reserved file %q", offlineArtifactCacheReadyFile)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("inspect HTTPS offline artifact archive cache marker: %w", err)
	}

	if err := os.Rename(bundleRoot, cacheDir); err != nil {
		if isOfflineArchiveCacheReady(cacheDir) {
			return cacheDir, nil
		}

		return "", fmt.Errorf("install HTTPS offline artifact cache: %w", err)
	}

	return cacheDir, nil
}

func isOfflineArchiveCacheReady(cacheDir string) bool {
	info, err := os.Stat(filepath.Join(cacheDir, offlineArtifactCacheReadyFile))

	return err == nil && info.Mode().IsRegular()
}

func markOfflineArchiveCacheReady(cacheDir string) error {
	path := filepath.Join(cacheDir, offlineArtifactCacheReadyFile)
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		return fmt.Errorf("mark HTTPS offline artifact cache ready: %w", err)
	}

	return nil
}

func findOfflineArchiveBundleRoot(extractDir string) (string, error) {
	var manifests []string

	if err := filepath.WalkDir(extractDir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if !entry.IsDir() && entry.Name() == OfflineArtifactManifestFileName {
			manifests = append(manifests, path)
		}

		return nil
	}); err != nil {
		return "", fmt.Errorf("inspect HTTPS offline artifact archive: %w", err)
	}

	if len(manifests) == 0 {
		return "", fmt.Errorf("HTTPS offline artifact archive does not contain %s", OfflineArtifactManifestFileName)
	}

	if len(manifests) > 1 {
		return "", fmt.Errorf("HTTPS offline artifact archive contains multiple %s files", OfflineArtifactManifestFileName)
	}

	return filepath.Dir(manifests[0]), nil
}

func loadOfflineManifest(sourceRoot string) (OfflineArtifactManifest, error) {
	var (
		data []byte
		err  error
	)

	if strings.HasPrefix(sourceRoot, "oci://") {
		data, err = fetchOCIBlobByTitle(context.Background(), sourceRoot, OfflineArtifactManifestFileName)
	} else {
		path := filepath.Join(sourceRoot, OfflineArtifactManifestFileName)
		data, err = os.ReadFile(path)
	}

	if err != nil {
		return OfflineArtifactManifest{}, fmt.Errorf("read offline artifact manifest from %q: %w", sourceRoot, err)
	}

	var manifest OfflineArtifactManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return OfflineArtifactManifest{}, fmt.Errorf("parse offline artifact manifest from %q: %w", sourceRoot, err)
	}

	manifest, err = normalizeOfflineManifest(manifest)
	if err != nil {
		return OfflineArtifactManifest{}, fmt.Errorf("validate offline artifact manifest from %q: %w", sourceRoot, err)
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

func verifyOfflineFiles(sourceRoot string, manifest OfflineArtifactManifest) error {
	paths := offlineArtifactPaths(manifest, runtime.GOARCH)
	if strings.HasPrefix(sourceRoot, "oci://") {
		return verifyOCIArtifacts(sourceRoot, paths)
	}

	var errs []error

	for _, path := range paths {
		fullPath := filepath.Join(sourceRoot, filepath.FromSlash(path))

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

func verifyOCIArtifacts(sourceRoot string, paths []string) error {
	manifest, err := ociartifact.FetchManifest(context.Background(), sourceRoot)
	if err != nil {
		return err
	}

	byTitle := ociartifact.DescriptorsByTitle(manifest)

	var errs []error

	for _, path := range paths {
		if _, ok := byTitle[path]; !ok {
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
	rootURL := offlineArtifactURLRoot(offlineArtifacts.SourceRoot)

	separator := "/"
	if strings.HasPrefix(offlineArtifacts.SourceRoot, "oci://") {
		separator = "#"
	}

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

	rootURL := offlineArtifactURLRoot(offlineArtifacts.SourceRoot)

	separator := "/"
	if strings.HasPrefix(offlineArtifacts.SourceRoot, "oci://") {
		separator = "#"
	}

	urls := make([]string, 0, len(offlineArtifacts.Manifest.ContainerImages))
	for _, imageTag := range offlineArtifacts.Manifest.ContainerImages {
		urls = append(urls, rootURL+separator+offlineContainerImageArchivePath(arch, imageTag))
	}

	return urls
}

func containerImageArchiveHostDir(sourceRoot string) string {
	return filepath.Join(ContainerImageArchiveHostSourceDir, containerImageArchiveSourceKey(sourceRoot))
}

func containerImageArchiveSourceKey(sourceRoot string) string {
	var b strings.Builder

	sourceIdentity := utilio.URLWithoutQuery(sourceRoot)
	for _, r := range strings.ToLower(sourceIdentity) {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}

	prefix := strings.Trim(b.String(), "-")
	if len(prefix) > 80 {
		prefix = strings.TrimRight(prefix[:80], "-")
	}

	if prefix == "" {
		prefix = "source"
	}

	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(sourceIdentity)))[:12]

	return prefix + "-" + hash
}

func offlineArtifactURLRoot(sourceRoot string) string {
	if strings.HasPrefix(sourceRoot, "oci://") {
		return sourceRoot
	}

	return fileURL(sourceRoot)
}

func fileURL(path string) string {
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(path)}).String()
}

func fetchOCIBlobByTitle(ctx context.Context, sourceRoot, title string) ([]byte, error) {
	body, err := ociartifact.Open(ctx, sourceRoot+"#"+title)
	if err != nil {
		return nil, err
	}
	defer body.Close() //nolint:errcheck // best effort close

	data, err := io.ReadAll(body)
	if err != nil {
		return nil, fmt.Errorf("read OCI blob %q: %w", title, err)
	}

	return data, nil
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
