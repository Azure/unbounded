// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package rootfs

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/preflight"
)

func remoteRootFSGoalState() *goalstates.RootFS {
	return &goalstates.RootFS{
		OCIImage:            "registry.example.com/unbounded/rootfs:v1",
		HostArch:            "amd64",
		KubernetesVersion:   "1.34.2",
		ContainerdVersion:   goalstates.ContainerdVersion,
		RunCVersion:         goalstates.RunCVersion,
		CNIPluginVersion:    goalstates.CNIPluginVersion,
		NSpawnConfigFile:    "/etc/systemd/nspawn/kube1.nspawn",
		ServiceOverrideFile: "/etc/systemd/system/systemd-nspawn@kube1.service.d/override.conf",
	}
}

func TestRemoteArtifactSourcesOmitChecksums(t *testing.T) {
	rootFS := remoteRootFSGoalState()

	kubeSources, err := kubernetesArtifactSources(rootFS)
	require.NoError(t, err)
	assert.Equal(t, []string{"kube-proxy", "kubectl", "kubelet"}, sourceNames(kubeSources))

	for _, source := range kubeSources {
		assert.NotContains(t, source.String(), ".sha256")
	}

	criSources, err := criArtifactSources(rootFS)
	require.NoError(t, err)
	assert.Equal(t, []string{"containerd", "crictl", "runc"}, sourceNames(criSources))

	cniSources, err := cniArtifactSources(rootFS)
	require.NoError(t, err)
	assert.Equal(t, []string{"cni-plugins"}, sourceNames(cniSources))
}

func TestRemoteArtifactCheckerUsesHeadBeforeGet(t *testing.T) {
	var (
		sawGet   bool
		gotRange string
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodHead:
			w.WriteHeader(http.StatusMethodNotAllowed)
		case http.MethodGet:
			sawGet = true
			gotRange = r.Header.Get("Range")

			w.WriteHeader(http.StatusPartialContent)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()

	rootFS := remoteRootFSGoalState()
	rootFS.Downloads = &goalstates.DownloadOverrides{
		CNI: &goalstates.DownloadSource{URL: server.URL + "/%s/%s/cni-plugins-v%s.tgz"},
	}

	results := CheckCNIArtifacts(slog.New(slog.DiscardHandler), rootFS).Check(context.Background())

	require.Len(t, results, 1)
	assert.Equal(t, preflight.SeverityOK, results[0].Severity)
	assert.True(t, sawGet)
	assert.Equal(t, "bytes=0-0", gotRange)
}

func TestRemoteArtifactCheckerCollectsAllFailures(t *testing.T) {
	checker := remoteArtifactChecker{
		log:        slog.New(slog.DiscardHandler),
		name:       checkKubernetesArtifactsName,
		target:     "kubernetes artifacts",
		errMessage: "one or more required Kubernetes artifact sources are not reachable",
		rootFS:     remoteRootFSGoalState(),
		sources: func(*goalstates.RootFS) (downloadArtifactSources, error) {
			return downloadArtifactSources{
				"kubelet":    mustDownloadSource(t, filepath.Join(t.TempDir(), "missing-kubelet")),
				"kubectl":    mustDownloadSource(t, writeProbeSource(t)),
				"kube-proxy": mustDownloadSource(t, filepath.Join(t.TempDir(), "missing-kube-proxy")),
			}, nil
		},
	}

	results := checker.Check(context.Background())

	require.Len(t, results, 2)
	assert.Equal(t, preflight.SeverityError, results[0].Severity)
	assert.Contains(t, results[0].Message, "kube-proxy")
	assert.Equal(t, preflight.SeverityError, results[1].Severity)
	assert.Contains(t, results[1].Message, "kubelet")
}

func TestRemoteArtifactCheckerRedactsFailedURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	rootFS := remoteRootFSGoalState()
	rootFS.Downloads = &goalstates.DownloadOverrides{
		CNI: &goalstates.DownloadSource{URL: server.URL + "/secret-token/%s/%s/cni-plugins-v%s.tgz"},
	}

	results := CheckCNIArtifacts(slog.New(slog.DiscardHandler), rootFS).Check(context.Background())

	require.Len(t, results, 1)
	assert.Equal(t, preflight.SeverityError, results[0].Severity)
	assert.Equal(t, checkCNIArtifactsName, results[0].Name)
	assert.Contains(t, results[0].Message, "cni-plugins")
	assert.NotContains(t, results[0].Message, server.URL)
	assert.NotContains(t, results[0].Message, "secret-token")
}

func TestCheckOCIImageReachableRequiresImage(t *testing.T) {
	rootFS := remoteRootFSGoalState()
	rootFS.OCIImage = ""

	results := CheckOCIImageReachable(slog.New(slog.DiscardHandler), rootFS).Check(context.Background())

	require.Len(t, results, 1)
	assert.Equal(t, preflight.SeverityError, results[0].Severity)
	assert.Equal(t, checkOCIImageReachableName, results[0].Name)
	assert.Equal(t, "OCI rootfs image is required but no image was selected", results[0].Message)
}

func writeProbeSource(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "artifact")
	require.NoError(t, os.WriteFile(path, []byte("ok"), 0o644))

	return path
}

func mustDownloadSource(t *testing.T, source string) downloadSource {
	t.Helper()

	parsed, err := parseDownloadSource(source)
	require.NoError(t, err)

	return parsed
}

func sourceNames(sources downloadArtifactSources) []string {
	names := sortedDownloadArtifactSourceNames(sources)
	for _, source := range sources {
		if strings.Contains(source.String(), ".sha256") {
			names = append(names, "unexpected-checksum")
		}
	}

	return names
}
