// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package bootstrapartifacts

import (
	"crypto/sha256"
	"fmt"
	"strings"
)

var KubernetesBinaries = []string{"kubelet", "kubectl", "kube-proxy"}

func KubernetesArtifactPath(version, arch, binary string) string {
	return fmt.Sprintf("kubernetes/%s/bin/linux/%s/%s", NormalizeKubernetesVersion(version), arch, binary)
}

func ContainerdArtifactPath(version, arch string) string {
	version = StripLeadingV(version)
	return fmt.Sprintf("containerd/v%s/containerd-%s-linux-%s.tar.gz", version, version, arch)
}

func RuncArtifactPath(version, arch string) string {
	return fmt.Sprintf("runc/v%s/runc.%s", StripLeadingV(version), arch)
}

func CNIArtifactPath(version, arch string) string {
	version = StripLeadingV(version)
	return fmt.Sprintf("cni/v%s/cni-plugins-linux-%s-v%s.tgz", version, arch, version)
}

func CrictlArtifactPath(version, hostOS, arch string) string {
	version = StripLeadingV(version)
	return fmt.Sprintf("crictl/v%s/crictl-v%s-%s-%s.tar.gz", version, version, hostOS, arch)
}

func CoreDNSArtifactPath(version, arch string) string {
	return fmt.Sprintf("coredns/v%s/bin/linux/%s/coredns", StripLeadingV(version), arch)
}

func ContainerImageArchivePath(arch, imageTag string) string {
	imageTag = strings.TrimSpace(imageTag)
	name := strings.NewReplacer(
		"/", "_",
		":", "_",
		"@", "_",
	).Replace(imageTag)
	digest := sha256.Sum256([]byte(imageTag))

	return fmt.Sprintf("container-images/%s/%s-%x.tar", arch, name, digest[:6])
}

// RequiredPaths returns the artifacts required by agent bootstrap for one host
// architecture.
func RequiredPaths(manifest Manifest, hostOS, arch string) []string {
	paths := []string{ManifestFileName}

	for _, binary := range KubernetesBinaries {
		path := KubernetesArtifactPath(manifest.Versions.Kubernetes, arch, binary)
		paths = append(paths, path, path+".sha256")
	}

	paths = append(paths,
		ContainerdArtifactPath(manifest.Versions.Containerd, arch),
		RuncArtifactPath(manifest.Versions.Runc, arch),
		CNIArtifactPath(manifest.Versions.CNI, arch),
		CrictlArtifactPath(manifest.Versions.Crictl, hostOS, arch),
	)

	if manifest.Versions.CoreDNS != "" {
		path := CoreDNSArtifactPath(manifest.Versions.CoreDNS, arch)
		paths = append(paths, path, path+".sha256")
	}

	for _, imageTag := range manifest.ContainerImages {
		path := ContainerImageArchivePath(arch, imageTag)
		paths = append(paths, path, path+".sha256")
	}

	return paths
}
