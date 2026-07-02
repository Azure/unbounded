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

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/retry"

	"github.com/Azure/unbounded/internal/ociutil"
	"github.com/Azure/unbounded/pkg/agent/config"
)

// OfflineArtifactManifestFileName is the manifest file name in an offline artifact bundle.
const OfflineArtifactManifestFileName = "manifest.json"

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

func ResolveOfflineArtifacts(cfg *config.AgentConfig, offline *config.AgentOfflineArtifacts) (*ResolvedOfflineArtifacts, error) {
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

	renderedSource, err := renderOfflineSource(offline.Source, clusterVersion)
	if err != nil {
		return nil, err
	}

	sourceRoot, err := normalizeOfflineSourceRoot(renderedSource)
	if err != nil {
		return nil, err
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

	return &ResolvedOfflineArtifacts{SourceRoot: sourceRoot, Manifest: manifest}, nil
}

func renderOfflineSource(sourceTemplate, kubernetesVersion string) (string, error) {
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
	manifest, err := fetchOCIManifest(context.Background(), sourceRoot)
	if err != nil {
		return err
	}

	byTitle := ociDescriptorsByTitle(manifest)

	var errs []error

	for _, path := range paths {
		if _, ok := byTitle[path]; !ok {
			errs = append(errs, fmt.Errorf("required offline artifact %q is missing", path))
		}
	}

	return errors.Join(errs...)
}

func downloadOverridesFromOfflineArtifacts(sourceRoot string, manifest OfflineArtifactManifest) *DownloadOverrides {
	rootURL := offlineArtifactURLRoot(sourceRoot)

	separator := "/"
	if strings.HasPrefix(sourceRoot, "oci://") {
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

func containerImageArchiveURLsFromOfflineArtifacts(sourceRoot string, manifest OfflineArtifactManifest, arch string) []string {
	rootURL := offlineArtifactURLRoot(sourceRoot)

	separator := "/"
	if strings.HasPrefix(sourceRoot, "oci://") {
		separator = "#"
	}

	urls := make([]string, 0, len(manifest.ContainerImages))
	for _, imageTag := range manifest.ContainerImages {
		urls = append(urls, rootURL+separator+offlineContainerImageArchivePath(arch, imageTag))
	}

	return urls
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
	manifest, err := fetchOCIManifest(ctx, sourceRoot)
	if err != nil {
		return nil, err
	}

	desc, ok := ociDescriptorsByTitle(manifest)[title]
	if !ok {
		return nil, fmt.Errorf("OCI artifact %q does not contain %q", sourceRoot, title)
	}

	repo, _, err := openOCIRepository(sourceRoot)
	if err != nil {
		return nil, err
	}

	body, err := repo.Fetch(ctx, desc)
	if err != nil {
		return nil, fmt.Errorf("fetch OCI blob %q: %w", title, err)
	}
	defer body.Close() //nolint:errcheck // best effort close

	data, err := io.ReadAll(body)
	if err != nil {
		return nil, fmt.Errorf("read OCI blob %q: %w", title, err)
	}

	return data, nil
}

func fetchOCIManifest(ctx context.Context, sourceRoot string) (ocispec.Manifest, error) {
	repo, ref, err := openOCIRepository(sourceRoot)
	if err != nil {
		return ocispec.Manifest{}, err
	}

	desc, err := repo.Resolve(ctx, ref)
	if err != nil {
		return ocispec.Manifest{}, fmt.Errorf("resolve OCI artifact %q: %w", sourceRoot, err)
	}

	return fetchOCIManifestDescriptor(ctx, repo, sourceRoot, desc)
}

func fetchOCIManifestDescriptor(ctx context.Context, repo *remote.Repository, sourceRoot string, desc ocispec.Descriptor) (ocispec.Manifest, error) {
	body, err := repo.Fetch(ctx, desc)
	if err != nil {
		return ocispec.Manifest{}, fmt.Errorf("fetch OCI artifact manifest %q: %w", sourceRoot, err)
	}
	defer body.Close() //nolint:errcheck // best effort close

	data, err := io.ReadAll(body)
	if err != nil {
		return ocispec.Manifest{}, fmt.Errorf("read OCI artifact manifest %q: %w", sourceRoot, err)
	}

	if desc.MediaType == ocispec.MediaTypeImageIndex {
		return fetchOCIPlatformManifest(ctx, repo, sourceRoot, data)
	}

	var manifest ocispec.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return ocispec.Manifest{}, fmt.Errorf("decode OCI artifact manifest %q: %w", sourceRoot, err)
	}

	if manifest.MediaType == ocispec.MediaTypeImageIndex {
		return fetchOCIPlatformManifest(ctx, repo, sourceRoot, data)
	}

	return manifest, nil
}

func fetchOCIPlatformManifest(ctx context.Context, repo *remote.Repository, sourceRoot string, data []byte) (ocispec.Manifest, error) {
	var index ocispec.Index
	if err := json.Unmarshal(data, &index); err != nil {
		return ocispec.Manifest{}, fmt.Errorf("decode OCI artifact index %q: %w", sourceRoot, err)
	}

	platformDesc, err := selectOCIPlatformManifest(sourceRoot, index)
	if err != nil {
		return ocispec.Manifest{}, err
	}

	return fetchOCIManifestDescriptor(ctx, repo, sourceRoot, platformDesc)
}

func selectOCIPlatformManifest(sourceRoot string, index ocispec.Index) (ocispec.Descriptor, error) {
	available := make([]string, 0, len(index.Manifests))
	for _, manifestDesc := range index.Manifests {
		if manifestDesc.Platform == nil {
			available = append(available, "<unknown>")
			continue
		}

		platform := manifestDesc.Platform
		available = append(available, platform.OS+"/"+platform.Architecture)

		if platform.OS == runtime.GOOS && platform.Architecture == runtime.GOARCH {
			return manifestDesc, nil
		}
	}

	return ocispec.Descriptor{}, fmt.Errorf("OCI artifact %q does not contain platform %s/%s (available: %s)", sourceRoot, runtime.GOOS, runtime.GOARCH, strings.Join(available, ", "))
}

func ociDescriptorsByTitle(manifest ocispec.Manifest) map[string]ocispec.Descriptor {
	out := make(map[string]ocispec.Descriptor, len(manifest.Layers))

	for _, desc := range manifest.Layers {
		title := desc.Annotations[ocispec.AnnotationTitle]
		if title != "" {
			out[title] = desc
		}
	}

	return out
}

func openOCIRepository(sourceRoot string) (*remote.Repository, string, error) {
	ref := strings.TrimPrefix(sourceRoot, "oci://")

	name, reference, err := splitOCIReference(ref)
	if err != nil {
		return nil, "", err
	}

	repo, err := remote.NewRepository(name)
	if err != nil {
		return nil, "", fmt.Errorf("parse OCI repository %q: %w", name, err)
	}

	ociutil.ConfigurePlainHTTP(repo)
	repo.Client = &auth.Client{Client: retry.DefaultClient, Cache: auth.DefaultCache}

	return repo, reference, nil
}

func splitOCIReference(ref string) (name, reference string, err error) {
	if idx := strings.LastIndex(ref, "@"); idx != -1 {
		return ref[:idx], ref[idx+1:], nil
	}

	lastSlash := strings.LastIndex(ref, "/")

	lastColon := strings.LastIndex(ref, ":")
	if lastColon > lastSlash && lastColon != -1 {
		return ref[:lastColon], ref[lastColon+1:], nil
	}

	return "", "", fmt.Errorf("OCI artifact source %q must include a tag or digest", ref)
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
