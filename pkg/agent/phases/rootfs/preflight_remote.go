// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package rootfs

import (
	"context"
	"fmt"
	"log/slog"
	"runtime"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/phases/rootfs/oci"
	"github.com/Azure/unbounded/pkg/agent/preflight"
)

const (
	checkKubernetesArtifactsName = "kubernetes-artifacts"
	checkCRIArtifactsName        = "cri-artifacts"
	checkCNIArtifactsName        = "cni-artifacts"
)

type remoteArtifactKind string

const (
	remoteArtifactOCIManifest remoteArtifactKind = "oci-manifest"
	remoteArtifactHTTPObject  remoteArtifactKind = "http-object"
)

type remoteArtifactSource struct {
	kind   remoteArtifactKind
	name   string
	target string
	oci    *remoteOCIArtifact
	http   *remoteHTTPArtifact
}

type remoteOCIArtifact struct {
	image string
}

type remoteHTTPArtifact struct {
	url string
}

type remoteArtifactProbe func(context.Context, remoteArtifactSource) error

type remoteArtifactChecker struct {
	log        *slog.Logger
	name       string
	target     string
	okMessage  string
	errMessage string
	rootFS     *goalstates.RootFS
	sources    func(*goalstates.RootFS) ([]remoteArtifactSource, error)
	probe      remoteArtifactProbe
}

// CheckOCIImageReachable validates that the OCI rootfs image manifest can be
// resolved without pulling layers.
func CheckOCIImageReachable(log *slog.Logger, rootFS *goalstates.RootFS) preflight.Checker {
	return remoteArtifactChecker{
		log:        log,
		name:       checkOCIImageReachableName,
		target:     "rootfs image",
		okMessage:  "rootfs image manifest is reachable",
		errMessage: "rootfs image manifest is not reachable",
		rootFS:     rootFS,
		sources:    ociImageSources,
		probe:      probeRemoteArtifactSource,
	}
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
		probe:      probeRemoteArtifactSource,
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
		probe:      probeRemoteArtifactSource,
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
		probe:      probeRemoteArtifactSource,
	}
}

func (c remoteArtifactChecker) Name() string { return c.name }

func (c remoteArtifactChecker) Check(ctx context.Context) []preflight.Result {
	if c.rootFS == nil {
		return preflight.ResultsError(c.name, c.target, "goal state could not be resolved")
	}

	if c.name == checkOCIImageReachableName && c.rootFS.OCIImage == "" {
		return preflight.ResultsError(c.name, c.target, "OCI rootfs image is required but no image was selected")
	}

	sources, err := c.sources(c.rootFS)
	if err != nil {
		return preflight.ResultsError(c.name, c.target, "artifact sources could not be resolved")
	}

	var failures []preflight.Result

	for _, source := range sources {
		if err := c.probe(ctx, source); err != nil {
			c.log.Debug("preflight remote artifact probe failed", slog.String("check", c.name), slog.String("source", source.name))

			failures = append(failures, preflight.Error(c.name, c.target, "%s: %s", c.errMessage, source.name))
		}
	}

	if len(failures) > 0 {
		return failures
	}

	return preflight.ResultsOK(c.name, c.target, c.okMessage)
}

func ociImageSources(rootFS *goalstates.RootFS) ([]remoteArtifactSource, error) {
	if rootFS.OCIImage == "" {
		return nil, fmt.Errorf("empty OCI image")
	}

	return []remoteArtifactSource{
		{
			kind:   remoteArtifactOCIManifest,
			name:   "rootfs-image",
			target: "rootfs image",
			oci:    &remoteOCIArtifact{image: rootFS.OCIImage},
		},
	}, nil
}

func kubernetesArtifactSources(rootFS *goalstates.RootFS) ([]remoteArtifactSource, error) {
	override := kubernetesDownloadSource(rootFS)
	version := downloadSourceVersion(rootFS.KubernetesVersion, override)

	sources := make([]remoteArtifactSource, 0, len(requiredKubeBinaries))
	for _, binary := range requiredKubeBinaries {
		sources = append(sources, httpArtifactSource(binary, "kubernetes artifacts", kubernetesBinaryURL(override, version, rootFS.HostArch, binary)))
	}

	return sources, nil
}

func criArtifactSources(rootFS *goalstates.RootFS) ([]remoteArtifactSource, error) {
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

	return []remoteArtifactSource{
		httpArtifactSource("containerd", "CRI artifacts", containerdDownloadURL(containerdOverride, containerdVersion, rootFS.HostArch)),
		httpArtifactSource("runc", "CRI artifacts", runcDownloadURL(runcOverride, runcVersion, rootFS.HostArch)),
		httpArtifactSource("crictl", "CRI artifacts", crictlDownloadURL(crictlOverride, crictlVersion, runtime.GOOS, rootFS.HostArch)),
	}, nil
}

func cniArtifactSources(rootFS *goalstates.RootFS) ([]remoteArtifactSource, error) {
	override := cniDownloadSource(rootFS)
	version := downloadSourceVersion(rootFS.CNIPluginVersion, override)

	return []remoteArtifactSource{
		httpArtifactSource("cni-plugins", "CNI artifacts", cniDownloadURL(override, version, rootFS.HostArch)),
	}, nil
}

func httpArtifactSource(name, target, url string) remoteArtifactSource {
	return remoteArtifactSource{
		kind:   remoteArtifactHTTPObject,
		name:   name,
		target: target,
		http:   &remoteHTTPArtifact{url: url},
	}
}

func probeRemoteArtifactSource(ctx context.Context, source remoteArtifactSource) error {
	switch source.kind {
	case remoteArtifactOCIManifest:
		if source.oci == nil {
			return fmt.Errorf("missing OCI source")
		}

		return oci.CheckImageReachable(ctx, source.oci.image)
	case remoteArtifactHTTPObject:
		if source.http == nil {
			return fmt.Errorf("missing HTTP source")
		}

		return probeArtifactObject(ctx, source.http.url)
	default:
		return fmt.Errorf("unsupported remote artifact source kind %q", source.kind)
	}
}
