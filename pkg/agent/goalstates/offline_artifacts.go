// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package goalstates

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"text/template"

	"github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/internal/ociartifact"
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

func resolveOfflineArtifacts(cfg *config.AgentConfig, offline *config.AgentOfflineArtifacts) (*ResolvedOfflineArtifacts, error) {
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

		if !ociSourceHasReference(u) {
			return "", fmt.Errorf("OCI offline artifact source must include a tag or digest: %q", source)
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

		path, err := url.PathUnescape(u.Path)
		if err != nil {
			return "", fmt.Errorf("unescape file offline artifact source path %q: %w", u.Path, err)
		}

		if path == "" || !filepath.IsAbs(path) {
			return "", fmt.Errorf("file offline artifact source must use an absolute path: %q", source)
		}

		return filepath.Clean(path), nil
	}

	if strings.Contains(source, "://") {
		return "", fmt.Errorf("unsupported OfflineArtifacts.Source scheme in %q", source)
	}

	if !filepath.IsAbs(source) {
		return "", fmt.Errorf("offline artifact source must be an absolute path: %q", source)
	}

	return filepath.Clean(source), nil
}

func ociSourceHasReference(u *url.URL) bool {
	path := strings.Trim(u.Path, "/")
	lastSlash := strings.LastIndex(path, "/")
	lastColon := strings.LastIndex(path, ":")

	return strings.Contains(path, "@") || lastColon > lastSlash
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

// ResolveDownloadOverridesWithOfflineArtifacts resolves cfg.OfflineArtifacts
// and returns download overrides that point at the offline artifact source. When
// OfflineArtifacts is not configured, the input downloads are returned unchanged.
// cfg must be non-nil.
func ResolveDownloadOverridesWithOfflineArtifacts(cfg *config.AgentConfig, downloads *DownloadOverrides) (*DownloadOverrides, error) {
	if cfg.OfflineArtifacts == nil || strings.TrimSpace(cfg.OfflineArtifacts.Source) == "" {
		return downloads, nil
	}

	resolved, err := resolveOfflineArtifacts(cfg, cfg.OfflineArtifacts)
	if err != nil {
		return nil, fmt.Errorf("resolve bootstrap artifact sources: %w", err)
	}

	return downloadOverridesFromOfflineArtifacts(resolved), nil
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
