// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package rootfs

import (
	"context"
	"fmt"
	"log/slog"
	"runtime"
	"sort"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/phases/rootfs/oci"
	"github.com/Azure/unbounded/pkg/agent/preflight"
)

const (
	checkKubernetesArtifactsName = "kubernetes-artifacts"
	checkCRIArtifactsName        = "cri-artifacts"
	checkCNIArtifactsName        = "cni-artifacts"
)

type downloadArtifactSources map[string]downloadSource

type ociImageReachableChecker struct {
	rootFS *goalstates.RootFS
}

type remoteArtifactChecker struct {
	log        *slog.Logger
	name       string
	target     string
	okMessage  string
	errMessage string
	rootFS     *goalstates.RootFS
	sources    func(*goalstates.RootFS) (downloadArtifactSources, error)
}

// CheckOCIImageReachable validates that the OCI rootfs image manifest can be
// resolved without pulling layers.
func CheckOCIImageReachable(_ *slog.Logger, rootFS *goalstates.RootFS) preflight.Checker {
	return ociImageReachableChecker{rootFS: rootFS}
}

func (c ociImageReachableChecker) Name() string { return checkOCIImageReachableName }

func (c ociImageReachableChecker) Check(ctx context.Context) []preflight.Result {
	if c.rootFS == nil {
		return preflight.ResultsError(checkOCIImageReachableName, "rootfs image", "goal state could not be resolved")
	}

	if c.rootFS.OCIImage == "" {
		return preflight.ResultsError(checkOCIImageReachableName, "rootfs image", "OCI rootfs image is required but no image was selected")
	}

	if err := oci.CheckImageReachable(ctx, c.rootFS.OCIImage); err != nil {
		return preflight.ResultsError(checkOCIImageReachableName, "rootfs image", "rootfs image manifest is not reachable")
	}

	return preflight.ResultsOK(checkOCIImageReachableName, "rootfs image", "rootfs image manifest is reachable")
}

// CheckKubernetesArtifacts validates that Kubernetes binary artifact URLs are
// reachable without downloading the full binaries or checksum files.
func CheckKubernetesArtifacts(log *slog.Logger, rootFS *goalstates.RootFS) preflight.Checker {
	return remoteArtifactChecker{
		log:        log,
		name:       checkKubernetesArtifactsName,
		target:     "kubernetes artifacts",
		okMessage:  "Kubernetes artifact sources are reachable",
		errMessage: "one or more required Kubernetes artifact sources are not reachable",
		rootFS:     rootFS,
		sources:    kubernetesArtifactSources,
	}
}

// CheckCRIArtifacts validates that CRI artifact URLs are reachable without
// downloading or extracting the full artifacts.
func CheckCRIArtifacts(log *slog.Logger, rootFS *goalstates.RootFS) preflight.Checker {
	return remoteArtifactChecker{
		log:        log,
		name:       checkCRIArtifactsName,
		target:     "CRI artifacts",
		okMessage:  "CRI artifact sources are reachable",
		errMessage: "one or more required CRI artifact sources are not reachable",
		rootFS:     rootFS,
		sources:    criArtifactSources,
	}
}

// CheckCNIArtifacts validates that CNI artifact URLs are reachable without
// downloading or extracting the full artifact.
func CheckCNIArtifacts(log *slog.Logger, rootFS *goalstates.RootFS) preflight.Checker {
	return remoteArtifactChecker{
		log:        log,
		name:       checkCNIArtifactsName,
		target:     "CNI artifacts",
		okMessage:  "CNI artifact sources are reachable",
		errMessage: "required CNI artifact source is not reachable",
		rootFS:     rootFS,
		sources:    cniArtifactSources,
	}
}

func (c remoteArtifactChecker) Name() string { return c.name }

func (c remoteArtifactChecker) Check(ctx context.Context) []preflight.Result {
	if c.rootFS == nil {
		return preflight.ResultsError(c.name, c.target, "goal state could not be resolved")
	}

	sources, err := c.sources(c.rootFS)
	if err != nil {
		return preflight.ResultsError(c.name, c.target, "artifact sources could not be resolved")
	}

	var failures []preflight.Result

	for _, sourceName := range sortedDownloadArtifactSourceNames(sources) {
		source := sources[sourceName]
		if err := source.probe(ctx); err != nil {
			c.log.Debug("preflight remote artifact probe failed", slog.String("check", c.name), slog.String("source", sourceName))

			failures = append(failures, preflight.Error(c.name, c.target, "%s: %s", c.errMessage, sourceName))
		}
	}

	if len(failures) > 0 {
		return failures
	}

	return preflight.ResultsOK(c.name, c.target, c.okMessage)
}

func sortedDownloadArtifactSourceNames(sources downloadArtifactSources) []string {
	names := make([]string, 0, len(sources))
	for name := range sources {
		names = append(names, name)
	}

	sort.Strings(names)

	return names
}

func kubernetesArtifactSources(rootFS *goalstates.RootFS) (downloadArtifactSources, error) {
	override := kubernetesDownloadSource(rootFS)
	version := downloadSourceVersion(rootFS.KubernetesVersion, override)

	sources := make(downloadArtifactSources, len(requiredKubeBinaries))
	for _, binary := range requiredKubeBinaries {
		source, err := kubernetesBinaryURL(override, version, rootFS.HostArch, binary)
		if err != nil {
			return nil, fmt.Errorf("resolve kubernetes binary source %q: %w", binary, err)
		}

		sources[binary] = source
	}

	return sources, nil
}

func criArtifactSources(rootFS *goalstates.RootFS) (downloadArtifactSources, error) {
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

	return downloadArtifactSources{
		"containerd": containerdSource,
		"runc":       runcSource,
		"crictl":     crictlSource,
	}, nil
}

func cniArtifactSources(rootFS *goalstates.RootFS) (downloadArtifactSources, error) {
	override := cniDownloadSource(rootFS)
	version := downloadSourceVersion(rootFS.CNIPluginVersion, override)

	source, err := cniDownloadURL(override, version, rootFS.HostArch)
	if err != nil {
		return nil, fmt.Errorf("resolve CNI plugin source: %w", err)
	}

	return downloadArtifactSources{"cni-plugins": source}, nil
}
