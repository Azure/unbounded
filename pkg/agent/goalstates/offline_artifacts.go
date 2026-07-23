// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package goalstates

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"text/template"

	"github.com/Azure/unbounded/pkg/agent/bootstrapartifacts"
	"github.com/Azure/unbounded/pkg/agent/config"
)

// OfflineArtifactManifestFileName is the manifest file name in an offline artifact bundle.
const OfflineArtifactManifestFileName = bootstrapartifacts.ManifestFileName

// OfflineArtifactVersions records the component versions included in an offline artifact bundle.
type OfflineArtifactVersions = bootstrapartifacts.Versions

// OfflineArtifactManifest describes an offline artifact bundle.
type OfflineArtifactManifest = bootstrapartifacts.Manifest

// OfflineTemplateData is the data available when rendering OfflineArtifacts.Source.
type OfflineTemplateData struct {
	KubernetesVersion    string
	KubernetesVersionNoV string
}

// ResolvedOfflineArtifacts is a validated offline artifact source and manifest.
type ResolvedOfflineArtifacts struct {
	SourceRoot string
	Manifest   OfflineArtifactManifest
	bundle     bootstrapartifacts.Bundle
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

	bundle, markValidated, err := bootstrapartifacts.Resolve(ctx, renderedSource, bootstrapartifacts.ResolveOptions{
		HTTPSArchiveRoot: OfflineArtifactArchiveHostDir,
	})
	if err != nil {
		return nil, err
	}

	manifest, err := loadOfflineManifest(ctx, bundle)
	if err != nil {
		return nil, err
	}

	if manifest.Versions.Kubernetes != clusterVersion {
		return nil, fmt.Errorf("offline artifacts Kubernetes version %q does not match Cluster.Version %q", manifest.Versions.Kubernetes, clusterVersion)
	}

	if err := validateRuntimeVersionConflicts(cfg, manifest); err != nil {
		return nil, err
	}

	if err := verifyOfflineFiles(ctx, bundle, manifest); err != nil {
		return nil, err
	}

	if markValidated != nil {
		if err := markValidated(); err != nil {
			return nil, err
		}
	}

	return &ResolvedOfflineArtifacts{SourceRoot: bundle.Root(), Manifest: manifest, bundle: bundle}, nil
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

func loadOfflineManifest(ctx context.Context, bundle bootstrapartifacts.Bundle) (OfflineArtifactManifest, error) {
	manifestSource, err := bundle.Artifact(OfflineArtifactManifestFileName)
	if err != nil {
		return OfflineArtifactManifest{}, fmt.Errorf("resolve offline artifact manifest source: %w", err)
	}

	data, err := manifestSource.ReadAll(ctx)
	if err != nil {
		return OfflineArtifactManifest{}, fmt.Errorf("read offline artifact manifest from %q: %w", bundle.Root(), err)
	}

	var manifest OfflineArtifactManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return OfflineArtifactManifest{}, fmt.Errorf("parse offline artifact manifest from %q: %w", bundle.Root(), err)
	}

	manifest, err = normalizeOfflineManifest(manifest)
	if err != nil {
		return OfflineArtifactManifest{}, fmt.Errorf("validate offline artifact manifest from %q: %w", bundle.Root(), err)
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

func verifyOfflineFiles(ctx context.Context, bundle bootstrapartifacts.Bundle, manifest OfflineArtifactManifest) error {
	paths := offlineArtifactPaths(manifest, runtime.GOARCH)

	diff, err := bootstrapartifacts.CompareContents(ctx, bundle, paths)
	if err != nil {
		return err
	}

	var errs []error
	for _, path := range diff.Missing {
		errs = append(errs, fmt.Errorf("required offline artifact %q is missing", path))
	}

	return errors.Join(errs...)
}

func offlineArtifactPaths(manifest OfflineArtifactManifest, arch string) []string {
	return bootstrapartifacts.RequiredPaths(manifest, "linux", arch)
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

	overrides := &DownloadOverrides{
		Kubernetes: &DownloadSource{
			URL:     offlineArtifacts.bundle.ArtifactURL("kubernetes/v%s/bin/linux/%s/%s"),
			Version: stripLeadingV(manifest.Versions.Kubernetes),
		},
		Containerd: &DownloadSource{
			URL:     offlineArtifacts.bundle.ArtifactURL("containerd/v%s/containerd-%s-linux-%s.tar.gz"),
			Version: manifest.Versions.Containerd,
		},
		Runc: &DownloadSource{
			URL:     offlineArtifacts.bundle.ArtifactURL("runc/v%s/runc.%s"),
			Version: manifest.Versions.Runc,
		},
		CNI: &DownloadSource{
			URL:     offlineArtifacts.bundle.ArtifactURL("cni/v%s/cni-plugins-linux-%s-v%s.tgz"),
			Version: manifest.Versions.CNI,
		},
		Crictl: &DownloadSource{
			URL:     offlineArtifacts.bundle.ArtifactURL("crictl/v%s/crictl-v%s-%s-%s.tar.gz"),
			Version: manifest.Versions.Crictl,
		},
	}

	return overrides
}

func containerImageArchiveURLsFromOfflineArtifacts(offlineArtifacts *ResolvedOfflineArtifacts, arch string) []string {
	if offlineArtifacts == nil {
		return []string{}
	}

	urls := make([]string, 0, len(offlineArtifacts.Manifest.ContainerImages))
	for _, imageTag := range offlineArtifacts.Manifest.ContainerImages {
		urls = append(urls, offlineArtifacts.bundle.ArtifactURL(offlineContainerImageArchivePath(arch, imageTag)))
	}

	return urls
}

func containerImageArchiveHostDir(sourceRoot string) string {
	return filepath.Join(ContainerImageArchiveHostSourceDir, bootstrapartifacts.SourceKey(sourceRoot))
}

func offlineContainerImageArchivePath(arch, imageTag string) string {
	return bootstrapartifacts.ContainerImageArchivePath(arch, imageTag)
}

func normalizeOfflineManifest(manifest OfflineArtifactManifest) (OfflineArtifactManifest, error) {
	return bootstrapartifacts.NormalizeManifest(manifest)
}

func normalizeKubernetesVersion(version string) string {
	return bootstrapartifacts.NormalizeKubernetesVersion(version)
}

func stripLeadingV(version string) string {
	return bootstrapartifacts.StripLeadingV(version)
}
