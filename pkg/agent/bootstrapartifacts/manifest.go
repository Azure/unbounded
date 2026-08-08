// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package bootstrapartifacts defines and reads complete agent bootstrap
// artifact bundles.
package bootstrapartifacts

import (
	"fmt"
	"sort"
	"strings"
)

const ManifestFileName = "manifest.json"

// Versions records component versions in a bootstrap artifact bundle.
type Versions struct {
	Kubernetes string `json:"kubernetes"`
	Containerd string `json:"containerd"`
	Runc       string `json:"runc"`
	CNI        string `json:"cni"`
	Crictl     string `json:"crictl"`
	CoreDNS    string `json:"coredns,omitempty"`
}

// Manifest describes a complete bootstrap artifact bundle.
type Manifest struct {
	SchemaVersion   int      `json:"schemaVersion,omitempty"`
	Versions        Versions `json:"versions"`
	ContainerImages []string `json:"containerImages"`
}

// NormalizeManifest validates and normalizes a bootstrap artifact manifest.
func NormalizeManifest(manifest Manifest) (Manifest, error) {
	if manifest.SchemaVersion == 0 {
		manifest.SchemaVersion = 1
	}

	if manifest.SchemaVersion != 1 {
		return Manifest{}, fmt.Errorf("unsupported manifest schemaVersion %d", manifest.SchemaVersion)
	}

	manifest.Versions.Kubernetes = NormalizeKubernetesVersion(manifest.Versions.Kubernetes)
	manifest.Versions.Containerd = StripLeadingV(manifest.Versions.Containerd)
	manifest.Versions.Runc = StripLeadingV(manifest.Versions.Runc)
	manifest.Versions.CNI = StripLeadingV(manifest.Versions.CNI)
	manifest.Versions.Crictl = StripLeadingV(manifest.Versions.Crictl)
	manifest.Versions.CoreDNS = StripLeadingV(manifest.Versions.CoreDNS)
	manifest.ContainerImages = NormalizeContainerImages(manifest.ContainerImages)

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
		return Manifest{}, fmt.Errorf("manifest is missing required fields: %s", strings.Join(missing, ", "))
	}

	return manifest, nil
}

func NormalizeKubernetesVersion(version string) string {
	version = strings.TrimSpace(version)
	if strings.HasPrefix(version, "v") {
		return version
	}

	return "v" + version
}

func StripLeadingV(version string) string {
	return strings.TrimPrefix(strings.TrimSpace(version), "v")
}

func NormalizeContainerImages(images []string) []string {
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
