// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package rootfs

import (
	"context"
	"fmt"
	"log/slog"
	"runtime"

	"github.com/Azure/unbounded/internal/agentartifacts"
	"github.com/Azure/unbounded/pkg/agent/artifactsource"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/phases/rootfs/oci"
	"github.com/Azure/unbounded/pkg/agent/preflight"
)

const (
	checkKubernetesArtifactsName = "kubernetes-artifacts"
	checkCRIArtifactsName        = "cri-artifacts"
	checkCNIArtifactsName        = "cni-artifacts"
	checkLocalDNSArtifactName    = "localdns-artifact"
)

type ociImageReachableChecker struct {
	rootFS *goalstates.RootFS
}

// CheckOCIImageReachable validates that the OCI rootfs image source is
// reachable without pulling image contents.
func CheckOCIImageReachable(_ *slog.Logger, rootFS *goalstates.RootFS) preflight.Checker {
	return ociImageReachableChecker{rootFS: rootFS}
}

func (c ociImageReachableChecker) Name() string { return checkOCIImageReachableName }

func (c ociImageReachableChecker) Check(ctx context.Context) []preflight.Result {
	if c.rootFS.OCIImage == "" {
		return preflight.ResultsError(checkOCIImageReachableName, "rootfs image", "OCI rootfs image is required but no image was selected")
	}

	if err := oci.CheckImageReachable(ctx, c.rootFS.OCIImage); err != nil {
		return preflight.ResultsError(checkOCIImageReachableName, "rootfs image", "rootfs image source is not reachable")
	}

	return preflight.ResultsOK(checkOCIImageReachableName, "rootfs image", "rootfs image source is reachable")
}

// CheckKubernetesArtifacts validates that Kubernetes binary artifact URLs are
// reachable without downloading the full binaries or checksum files.
func CheckKubernetesArtifacts(log *slog.Logger, rootFS *goalstates.RootFS) preflight.Checker {
	return artifactsource.ReachabilityChecker{
		Log:        log,
		CheckName:  checkKubernetesArtifactsName,
		Target:     "kubernetes artifacts",
		OKMessage:  "Kubernetes artifact sources are reachable",
		ErrMessage: "one or more required Kubernetes artifact sources are not reachable",
		Sources: func() (artifactsource.Sources, error) {
			return kubernetesArtifactSources(rootFS)
		},
	}
}

// CheckCRIArtifacts validates that CRI artifact URLs are reachable without
// downloading or extracting the full artifacts.
func CheckCRIArtifacts(log *slog.Logger, rootFS *goalstates.RootFS) preflight.Checker {
	return artifactsource.ReachabilityChecker{
		Log:        log,
		CheckName:  checkCRIArtifactsName,
		Target:     "CRI artifacts",
		OKMessage:  "CRI artifact sources are reachable",
		ErrMessage: "one or more required CRI artifact sources are not reachable",
		Sources: func() (artifactsource.Sources, error) {
			return criArtifactSources(rootFS)
		},
	}
}

// CheckLocalDNSArtifact validates that the selected CoreDNS artifact is reachable.
func CheckLocalDNSArtifact(log *slog.Logger, rootFS *goalstates.RootFS) preflight.Checker {
	return artifactsource.ReachabilityChecker{
		Log:        log,
		CheckName:  checkLocalDNSArtifactName,
		Target:     "LocalDNS artifact",
		OKMessage:  "CoreDNS artifact source is reachable",
		ErrMessage: "CoreDNS artifact source is not reachable",
		Sources: func() (artifactsource.Sources, error) {
			override := coreDNSDownloadSource(rootFS)

			source, err := artifactsource.Parse(agentartifacts.CoreDNSArchive(override, rootFS.LocalDNS.CoreDNSVersion, rootFS.HostArch))
			if err != nil {
				return nil, err
			}

			return artifactsource.Sources{"coredns": source}, nil
		},
	}
}

// CheckCNIArtifacts validates that CNI artifact URLs are reachable without
// downloading or extracting the full artifact.
func CheckCNIArtifacts(log *slog.Logger, rootFS *goalstates.RootFS) preflight.Checker {
	return artifactsource.ReachabilityChecker{
		Log:        log,
		CheckName:  checkCNIArtifactsName,
		Target:     "CNI artifacts",
		OKMessage:  "CNI artifact sources are reachable",
		ErrMessage: "required CNI artifact source is not reachable",
		Sources: func() (artifactsource.Sources, error) {
			return cniArtifactSources(rootFS)
		},
	}
}

func kubernetesArtifactSources(rootFS *goalstates.RootFS) (artifactsource.Sources, error) {
	override := kubernetesDownloadSource(rootFS)
	version := downloadSourceVersion(rootFS.KubernetesVersion, override)

	sources := make(artifactsource.Sources, len(requiredKubeBinaries))
	for _, binary := range requiredKubeBinaries {
		source, err := kubernetesBinaryURL(override, version, rootFS.HostArch, binary)
		if err != nil {
			return nil, fmt.Errorf("resolve kubernetes binary source %q: %w", binary, err)
		}

		sources[binary] = source
	}

	return sources, nil
}

func criArtifactSources(rootFS *goalstates.RootFS) (artifactsource.Sources, error) {
	containerdOverride := containerdDownloadSource(rootFS)
	runcOverride := runcDownloadSource(rootFS)
	crictlOverride := crictlDownloadSource(rootFS)
	kubernetesVersion := downloadSourceVersion(rootFS.KubernetesVersion, kubernetesDownloadSource(rootFS))
	containerdVersion := downloadSourceVersion(rootFS.ContainerdVersion, containerdOverride)
	runcVersion := downloadSourceVersion(rootFS.RunCVersion, runcOverride)

	crictlVersion, err := resolveCrictlVersion(crictlOverride, kubernetesVersion)
	if err != nil {
		return nil, fmt.Errorf("resolve crictl version: %w", err)
	}

	containerdSource, err := containerdDownloadURL(containerdOverride, containerdVersion, rootFS.HostArch)
	if err != nil {
		return nil, fmt.Errorf("resolve containerd source: %w", err)
	}

	runcSource, err := runcDownloadURL(runcOverride, runcVersion, rootFS.HostArch)
	if err != nil {
		return nil, fmt.Errorf("resolve runc source: %w", err)
	}

	crictlSource, err := crictlDownloadURL(crictlOverride, crictlVersion, runtime.GOOS, rootFS.HostArch)
	if err != nil {
		return nil, fmt.Errorf("resolve crictl source: %w", err)
	}

	return artifactsource.Sources{
		"containerd": containerdSource,
		"runc":       runcSource,
		"crictl":     crictlSource,
	}, nil
}

func cniArtifactSources(rootFS *goalstates.RootFS) (artifactsource.Sources, error) {
	override := cniDownloadSource(rootFS)
	version := downloadSourceVersion(rootFS.CNIPluginVersion, override)

	source, err := cniDownloadURL(override, version, rootFS.HostArch)
	if err != nil {
		return nil, fmt.Errorf("resolve CNI plugin source: %w", err)
	}

	return artifactsource.Sources{"cni-plugins": source}, nil
}
