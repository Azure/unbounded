// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// release-bom generates a machine-readable inventory of release images and
// node bootstrap dependencies. Image tags are resolved to immutable digests so
// downstream automation can verify exactly what a release is expected to use.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/retry"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

const bomSchemaVersion = 1

var releaseImageNames = []string{
	"gantry",
	"host-ubuntu2404",
	"inventory-aggregator",
	"inventory-inspector",
	"inventory-viewer",
	"machina",
	"machine-ops-controller",
	"metalman",
	"netboot",
	"orca",
	"unbounded-net-controller",
	"unbounded-net-node",
	"unbounded-operator",
	"unbounded-storage-supervisor",
}

type options struct {
	tag              string
	commit           string
	registry         string
	netCNIVersion    string
	aclSysextSystemd string
	output           string
}

type releaseBOM struct {
	SchemaVersion int                  `json:"schemaVersion"`
	Release       releaseInfo          `json:"release"`
	Artifacts     []releaseArtifact    `json:"artifacts"`
	Images        []resolvedImage      `json:"images"`
	NodeBootstrap nodeBootstrapProfile `json:"nodeBootstrap"`
}

type releaseInfo struct {
	Tag       string `json:"tag"`
	GitCommit string `json:"gitCommit"`
}

type releaseArtifact struct {
	Name            string `json:"name"`
	Integrity       string `json:"integrity"`
	SignatureBundle string `json:"signatureBundle,omitempty"`
}

type resolvedImage struct {
	Name      string   `json:"name"`
	Reference string   `json:"reference"`
	Digest    string   `json:"digest"`
	MediaType string   `json:"mediaType"`
	Platforms []string `json:"platforms,omitempty"`
}

type nodeBootstrapProfile struct {
	KubernetesVersionSource string          `json:"kubernetesVersionSource"`
	ContainerdVersion       string          `json:"containerdVersion"`
	RuncVersion             string          `json:"runcVersion"`
	CNIPluginVersion        string          `json:"cniPluginVersion"`
	NetCNIPluginVersion     string          `json:"netCniPluginVersion"`
	CrictlVersionSource     string          `json:"crictlVersionSource"`
	SandboxImage            resolvedImage   `json:"sandboxImage"`
	KubeProxyImageTemplate  string          `json:"kubeProxyImageTemplate"`
	RootFSImages            []resolvedImage `json:"rootfsImages"`
}

type imageResolver func(context.Context, string, string) (resolvedImage, error)

func main() {
	var opts options

	flag.StringVar(&opts.tag, "tag", "", "Release tag")
	flag.StringVar(&opts.commit, "commit", "", "Release git commit")
	flag.StringVar(&opts.registry, "registry", "ghcr.io/azure", "Container registry and repository prefix")
	flag.StringVar(&opts.netCNIVersion, "net-cni-version", "", "CNI plugin bundle version in the unbounded-net node image")
	flag.StringVar(&opts.aclSysextSystemd, "acl-sysext-systemd", "", "systemd release the Azure Container Linux nspawn system extension targets (e.g. 255-33.azl3)")
	flag.StringVar(&opts.output, "output", "", "Output JSON file")
	flag.Parse()

	if err := opts.validate(); err != nil {
		exitWithError(err)
	}

	bom, err := buildBOM(context.Background(), opts, resolveImage)
	if err != nil {
		exitWithError(err)
	}

	if err := writeBOM(opts.output, bom); err != nil {
		exitWithError(err)
	}
}

func (o options) validate() error {
	switch {
	case strings.TrimSpace(o.tag) == "":
		return fmt.Errorf("--tag is required")
	case strings.TrimSpace(o.commit) == "":
		return fmt.Errorf("--commit is required")
	case strings.TrimSpace(o.registry) == "":
		return fmt.Errorf("--registry is required")
	case strings.TrimSpace(o.netCNIVersion) == "":
		return fmt.Errorf("--net-cni-version is required")
	case strings.TrimSpace(o.output) == "":
		return fmt.Errorf("--output is required")
	default:
		return nil
	}
}

func buildBOM(ctx context.Context, opts options, resolve imageResolver) (*releaseBOM, error) {
	images := make([]resolvedImage, 0, len(releaseImageNames))
	for _, name := range releaseImageNames {
		ref := strings.TrimRight(opts.registry, "/") + "/" + name + ":" + opts.tag

		image, err := resolve(ctx, name, ref)
		if err != nil {
			return nil, fmt.Errorf("resolve release image %s: %w", name, err)
		}

		images = append(images, image)
	}

	rootFSRefs := map[string]string{
		"azurelinux3":        goalstates.DefaultAzureLinux3OCIImage,
		"azurelinux3-nvidia": goalstates.DefaultAzureLinux3NvidiaOCIImage,
		"ubuntu2404":         goalstates.DefaultOCIImage,
		"ubuntu2404-nvidia":  goalstates.DefaultNvidiaOCIImage,
		"ubuntu2604":         goalstates.DefaultUbuntu2604OCIImage,
		"ubuntu2604-nvidia":  goalstates.DefaultUbuntu2604NvidiaOCIImage,
	}

	rootFSNames := make([]string, 0, len(rootFSRefs))
	for name := range rootFSRefs {
		rootFSNames = append(rootFSNames, name)
	}

	sort.Strings(rootFSNames)

	rootFSImages := make([]resolvedImage, 0, len(rootFSNames))
	for _, name := range rootFSNames {
		image, err := resolve(ctx, name, rootFSRefs[name])
		if err != nil {
			return nil, fmt.Errorf("resolve rootfs image %s: %w", name, err)
		}

		rootFSImages = append(rootFSImages, image)
	}

	sandboxImage, err := resolve(ctx, "sandbox", goalstates.SandboxImage)
	if err != nil {
		return nil, fmt.Errorf("resolve sandbox image: %w", err)
	}

	return &releaseBOM{
		SchemaVersion: bomSchemaVersion,
		Release: releaseInfo{
			Tag:       opts.tag,
			GitCommit: opts.commit,
		},
		Artifacts: releaseArtifacts(opts.tag, opts.aclSysextSystemd),
		Images:    images,
		NodeBootstrap: nodeBootstrapProfile{
			KubernetesVersionSource: "cluster control plane version",
			ContainerdVersion:       goalstates.ContainerdVersion,
			RuncVersion:             goalstates.RunCVersion,
			CNIPluginVersion:        goalstates.CNIPluginVersion,
			NetCNIPluginVersion:     opts.netCNIVersion,
			CrictlVersionSource:     "Kubernetes major.minor.0",
			SandboxImage:            sandboxImage,
			KubeProxyImageTemplate:  goalstates.KubeProxyImageRepository + ":v{KUBERNETES_VERSION}",
			RootFSImages:            rootFSImages,
		},
	}, nil
}

func releaseArtifacts(tag, aclSysextSystemd string) []releaseArtifact {
	artifacts := []releaseArtifact{
		{Name: "checksums.txt", Integrity: "cosign-bundle", SignatureBundle: "checksums.txt.bundle.json"},
		{Name: "unbounded-manifests-" + tag + ".tar.gz", Integrity: "cosign-bundle", SignatureBundle: "unbounded-manifests-" + tag + ".tar.gz.bundle.json"},
		{Name: "unbounded-operator-" + tag + ".yaml", Integrity: "cosign-bundle", SignatureBundle: "unbounded-operator-" + tag + ".yaml.bundle.json"},
		{Name: "unbounded-storage-linux-amd64.tar.gz", Integrity: "sha256-and-cosign-bundle", SignatureBundle: "unbounded-storage-linux-amd64.tar.gz.bundle.json"},
		{Name: "unbounded-storage-linux-arm64.tar.gz", Integrity: "sha256-and-cosign-bundle", SignatureBundle: "unbounded-storage-linux-arm64.tar.gz.bundle.json"},
		{Name: "unbounded.yaml", Integrity: "contains-archive-sha256"},
	}

	// The Azure Container Linux system extension is named for the systemd
	// release it targets rather than for this release, so it can only be listed
	// when the caller supplies that version.
	if aclSysextSystemd != "" {
		for _, arch := range []string{"amd64", "arm64"} {
			name := fmt.Sprintf("unbounded-nspawn-systemd-%s-linux-%s.raw", aclSysextSystemd, arch)
			artifacts = append(artifacts, releaseArtifact{
				Name:            name,
				Integrity:       "sha256-and-cosign-bundle",
				SignatureBundle: name + ".bundle.json",
			})
		}
	}

	return artifacts
}

func resolveImage(ctx context.Context, name, ref string) (resolvedImage, error) {
	repo, err := remote.NewRepository(ref)
	if err != nil {
		return resolvedImage{}, fmt.Errorf("parse image reference %q: %w", ref, err)
	}

	repo.Client = &auth.Client{Client: retry.DefaultClient, Cache: auth.DefaultCache}

	desc, err := repo.Resolve(ctx, repo.Reference.Reference)
	if err != nil {
		return resolvedImage{}, fmt.Errorf("resolve image reference %q: %w", ref, err)
	}

	image := resolvedImage{
		Name:      name,
		Reference: ref,
		Digest:    desc.Digest.String(),
		MediaType: desc.MediaType,
	}

	if desc.MediaType != ocispec.MediaTypeImageIndex && desc.MediaType != "application/vnd.docker.distribution.manifest.list.v2+json" {
		return image, nil
	}

	content, err := repo.Fetch(ctx, desc)
	if err != nil {
		return resolvedImage{}, fmt.Errorf("fetch image index %q: %w", ref, err)
	}
	defer content.Close() //nolint:errcheck // best effort close

	var index ocispec.Index
	if err := json.NewDecoder(content).Decode(&index); err != nil {
		return resolvedImage{}, fmt.Errorf("decode image index %q: %w", ref, err)
	}

	var platforms []string

	for _, manifest := range index.Manifests {
		if manifest.Platform == nil || manifest.Platform.OS == "" || manifest.Platform.Architecture == "" || manifest.Platform.OS == "unknown" || manifest.Platform.Architecture == "unknown" {
			continue
		}

		platform := manifest.Platform.OS + "/" + manifest.Platform.Architecture
		if manifest.Platform.Variant != "" {
			platform += "/" + manifest.Platform.Variant
		}

		platforms = append(platforms, platform)
	}

	image.Platforms = uniqueSortedStrings(platforms)

	return image, nil
}

func uniqueSortedStrings(values []string) []string {
	unique := make(map[string]struct{}, len(values))
	for _, value := range values {
		unique[value] = struct{}{}
	}

	result := make([]string, 0, len(unique))
	for value := range unique {
		result = append(result, value)
	}

	sort.Strings(result)

	return result
}

func writeBOM(path string, bom *releaseBOM) error {
	var output io.Writer = os.Stdout

	if path != "-" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return fmt.Errorf("create BOM output directory: %w", err)
		}

		file, err := os.Create(path)
		if err != nil {
			return fmt.Errorf("create BOM %q: %w", path, err)
		}
		defer file.Close() //nolint:errcheck // best effort close

		output = file
	}

	encoder := json.NewEncoder(output)
	encoder.SetIndent("", "  ")
	encoder.SetEscapeHTML(false)

	if err := encoder.Encode(bom); err != nil {
		return fmt.Errorf("encode BOM: %w", err)
	}

	return nil
}

func exitWithError(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
