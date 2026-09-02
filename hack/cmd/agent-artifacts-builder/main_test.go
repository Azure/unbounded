// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

func TestRootfsImagesMatchAgentDefaults(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile("rootfs-images.txt")
	require.NoError(t, err)

	require.Equal(t, []string{
		goalstates.DefaultOCIImage,
		goalstates.DefaultNvidiaOCIImage,
		goalstates.DefaultUbuntu2604OCIImage,
		goalstates.DefaultUbuntu2604NvidiaOCIImage,
		goalstates.DefaultAzureLinux3OCIImage,
		goalstates.DefaultAzureLinux3NvidiaOCIImage,
	}, strings.Fields(stripLineComments(string(data))))
}

func TestKubernetesVersionsIncludeCurrentStablePatches(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile("kubernetes-versions.txt")
	require.NoError(t, err)

	versions := strings.Fields(stripLineComments(string(data)))
	for _, version := range []string{"v1.35.7", "v1.35.8", "v1.36.3", "v1.36.4"} {
		require.Contains(t, versions, version)
	}
}

func TestRunBootstrapArchivePipeline(t *testing.T) {
	versions := []string{"v1", "v2", "v3", "v4", "v5", "v6"}
	started := make(chan struct{}, len(versions))
	release := make(chan struct{})
	result := make(chan error, 1)

	var (
		active    atomic.Int32
		maxActive atomic.Int32
		cleaned   atomic.Int32
	)

	go func() {
		result <- runBootstrapArchivePipeline(
			context.Background(),
			versions,
			func(_ context.Context, version string) (bootstrapArchiveTask, error) {
				return bootstrapArchiveTask{
					archivePath: version,
					cleanup: func() {
						cleaned.Add(1)
					},
				}, nil
			},
			func(context.Context, bootstrapArchiveTask) error {
				current := active.Add(1)
				defer active.Add(-1)

				for {
					maximum := maxActive.Load()
					if current <= maximum || maxActive.CompareAndSwap(maximum, current) {
						break
					}
				}

				started <- struct{}{}

				<-release

				return nil
			},
		)
	}()

	for range bootstrapArchiveConcurrency {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("archive workers did not run concurrently")
		}
	}

	require.EqualValues(t, bootstrapArchiveConcurrency, maxActive.Load())
	close(release)
	require.NoError(t, <-result)
	require.EqualValues(t, len(versions), cleaned.Load())
}

func TestRunBootstrapArchivePipelinePropagatesPrepareFailure(t *testing.T) {
	var cleaned atomic.Int32

	prepareErr := errors.New("prepare failed")

	err := runBootstrapArchivePipeline(
		context.Background(),
		[]string{"good", "bad"},
		func(_ context.Context, version string) (bootstrapArchiveTask, error) {
			if version == "bad" {
				return bootstrapArchiveTask{}, prepareErr
			}

			return bootstrapArchiveTask{cleanup: func() { cleaned.Add(1) }}, nil
		},
		func(context.Context, bootstrapArchiveTask) error { return nil },
	)

	require.ErrorIs(t, err, prepareErr)
	require.EqualValues(t, 1, cleaned.Load())
}

func TestRunBootstrapArchivePipelinePropagatesArchiveFailure(t *testing.T) {
	var cleaned atomic.Int32

	archiveErr := errors.New("archive failed")

	err := runBootstrapArchivePipeline(
		context.Background(),
		[]string{"v1"},
		func(context.Context, string) (bootstrapArchiveTask, error) {
			return bootstrapArchiveTask{cleanup: func() { cleaned.Add(1) }}, nil
		},
		func(context.Context, bootstrapArchiveTask) error { return archiveErr },
	)

	require.ErrorIs(t, err, archiveErr)
	require.EqualValues(t, 1, cleaned.Load())
}

func TestResolvePublishInputsWorkflowDispatchUsesExplicitInputs(t *testing.T) {
	output := filepath.Join(t.TempDir(), "github-output")

	t.Setenv("EVENT_NAME", "workflow_dispatch")
	t.Setenv("REF_NAME", "")
	t.Setenv("INPUT_TAG", "v0.4.0-alpha")
	t.Setenv("INPUT_RELEASE_TAG", "v0.4.0-alpha")
	t.Setenv("INPUT_KUBERNETES_VERSIONS", "1.34.9, v1.35.6")
	t.Setenv("INPUT_ROOTFS_IMAGES", "ghcr.io/azure/agent-ubuntu2404:v1, ghcr.io/azure/agent-azlinux3:v2")
	t.Setenv("GITHUB_SHA_VALUE", "0123456789abcdef")
	t.Setenv("GITHUB_OUTPUT", output)
	t.Setenv("DEFAULT_KUBERNETES_VERSIONS_FILE", "")
	t.Setenv("DEFAULT_ROOTFS_IMAGES_FILE", "")

	require.NoError(t, resolvePublishInputs())

	values := readGitHubOutput(t, output)
	require.Equal(t, "v0.4.0-alpha", values["tag"])
	require.Equal(t, "v0.4.0-alpha", values["release_tag"])
	requireJSONEqual(t, []string{"v1.34.9", "v1.35.6"}, values["kubernetes_versions"])
	requireJSONEqual(t, []string{
		"ghcr.io/azure/agent-ubuntu2404:v1",
		"ghcr.io/azure/agent-azlinux3:v2",
	}, values["rootfs_images"])
}

func TestResolvePublishInputsWorkflowDispatchDefaultsTagAndVersions(t *testing.T) {
	dir := t.TempDir()
	versionsFile := filepath.Join(dir, "versions.txt")
	require.NoError(t, os.WriteFile(versionsFile, []byte(`
# current supported patches
1.34.8
v1.34.9
`), 0o644))

	rootfsImagesFile := filepath.Join(dir, "rootfs-images.txt")
	require.NoError(t, os.WriteFile(rootfsImagesFile, []byte("ghcr.io/azure/agent-ubuntu2404:v1\n"), 0o644))

	output := filepath.Join(dir, "github-output")

	t.Setenv("EVENT_NAME", "workflow_dispatch")
	t.Setenv("REF_NAME", "")
	t.Setenv("INPUT_TAG", "")
	t.Setenv("INPUT_RELEASE_TAG", "")
	t.Setenv("INPUT_KUBERNETES_VERSIONS", "")
	t.Setenv("INPUT_ROOTFS_IMAGES", "")
	t.Setenv("GITHUB_SHA_VALUE", "0123456789abcdef")
	t.Setenv("GITHUB_OUTPUT", output)
	t.Setenv("DEFAULT_KUBERNETES_VERSIONS_FILE", versionsFile)
	t.Setenv("DEFAULT_ROOTFS_IMAGES_FILE", rootfsImagesFile)

	require.NoError(t, resolvePublishInputs())

	values := readGitHubOutput(t, output)
	require.Equal(t, "0123456789ab", values["tag"])
	require.Empty(t, values["release_tag"])
	requireJSONEqual(t, []string{"v1.34.8", "v1.34.9"}, values["kubernetes_versions"])
	requireJSONEqual(t, []string{"ghcr.io/azure/agent-ubuntu2404:v1"}, values["rootfs_images"])
}

func TestResolvePublishInputsTagPushUsesRefTagAndDefaultVersions(t *testing.T) {
	dir := t.TempDir()
	versionsFile := filepath.Join(dir, "versions.txt")
	require.NoError(t, os.WriteFile(versionsFile, []byte("v1.35.5\nv1.35.6\n"), 0o644))

	rootfsImagesFile := filepath.Join(dir, "rootfs-images.txt")
	require.NoError(t, os.WriteFile(rootfsImagesFile, []byte("ghcr.io/azure/agent-azlinux3:v2\n"), 0o644))

	output := filepath.Join(dir, "github-output")

	t.Setenv("EVENT_NAME", "push")
	t.Setenv("REF_NAME", "v1.22.0")
	t.Setenv("INPUT_TAG", "ignored")
	t.Setenv("INPUT_RELEASE_TAG", "ignored")
	t.Setenv("INPUT_KUBERNETES_VERSIONS", "v1.34.9")
	t.Setenv("INPUT_ROOTFS_IMAGES", "ghcr.io/azure/ignored:v1")
	t.Setenv("GITHUB_SHA_VALUE", "0123456789abcdef")
	t.Setenv("GITHUB_OUTPUT", output)
	t.Setenv("DEFAULT_KUBERNETES_VERSIONS_FILE", versionsFile)
	t.Setenv("DEFAULT_ROOTFS_IMAGES_FILE", rootfsImagesFile)

	require.NoError(t, resolvePublishInputs())

	values := readGitHubOutput(t, output)
	require.Equal(t, "v1.22.0", values["tag"])
	require.Equal(t, "v1.22.0", values["release_tag"])
	requireJSONEqual(t, []string{"v1.35.5", "v1.35.6"}, values["kubernetes_versions"])
	requireJSONEqual(t, []string{"ghcr.io/azure/agent-azlinux3:v2"}, values["rootfs_images"])
}

func TestResolvePublishInputsRequiresTag(t *testing.T) {
	t.Setenv("EVENT_NAME", "workflow_dispatch")
	t.Setenv("REF_NAME", "")
	t.Setenv("INPUT_TAG", "")
	t.Setenv("INPUT_RELEASE_TAG", "")
	t.Setenv("INPUT_KUBERNETES_VERSIONS", "v1.34.9")
	t.Setenv("GITHUB_SHA_VALUE", "")
	t.Setenv("GITHUB_OUTPUT", filepath.Join(t.TempDir(), "github-output"))
	t.Setenv("DEFAULT_KUBERNETES_VERSIONS_FILE", "")

	require.ErrorContains(t, resolvePublishInputs(), "artifact tag could not be resolved")
}

func TestBuilderCommandVisibility(t *testing.T) {
	root := newRootCommand()

	commands := map[string]*cobra.Command{}
	for _, command := range root.Commands() {
		commands[command.Name()] = command
	}

	require.NotContains(t, commands, "publish-version-group")
	require.NotContains(t, commands, "archive-oci-image")
}

func TestPlanRootfsArchivesRejectsFilenameCollision(t *testing.T) {
	_, err := planRootfsArchives(t.TempDir(), []string{
		"ghcr.io/first/agent-ubuntu2404:v1",
		"ghcr.io/second/agent-ubuntu2404:v1",
	})
	require.ErrorContains(t, err, "produce the same archive name")
	require.ErrorContains(t, err, "rootfs-agent-ubuntu2404-v1.oci.tar.gz")
}

func TestResolveBuildInputsUseExplicitValues(t *testing.T) {
	versions, err := resolveBuildKubernetesVersions([]string{"1.34.9", "v1.35.6"})
	require.NoError(t, err)
	require.Equal(t, []string{"v1.34.9", "v1.35.6"}, versions)

	images, err := resolveBuildRootfsImages([]string{
		"ghcr.io/azure/agent-ubuntu2404:v1",
		"oci://ghcr.io/azure/agent-azlinux3:v2",
	})
	require.NoError(t, err)
	require.Equal(t, []string{
		"ghcr.io/azure/agent-ubuntu2404:v1",
		"oci://ghcr.io/azure/agent-azlinux3:v2",
	}, images)
}

func TestResolveBuildInputsUseEmbeddedDefaults(t *testing.T) {
	versions, err := resolveBuildKubernetesVersions(nil)
	require.NoError(t, err)
	require.NotEmpty(t, versions)

	images, err := resolveBuildRootfsImages(nil)
	require.NoError(t, err)
	require.NotEmpty(t, images)
}

func TestResolveBuildRootfsImagesRejectsDigest(t *testing.T) {
	_, err := resolveBuildRootfsImages([]string{"ghcr.io/azure/agent-ubuntu2404@sha256:0000000000000000000000000000000000000000000000000000000000000000"})
	require.ErrorContains(t, err, "must use a tag")
}

func TestNormalizeRootfsImages(t *testing.T) {
	t.Parallel()

	got, err := normalizeRootfsImages("ghcr.io/azure/agent-ubuntu2404:v1, oci://ghcr.io/azure/agent-azlinux3:v2")
	require.NoError(t, err)
	require.Equal(t, []string{
		"ghcr.io/azure/agent-ubuntu2404:v1",
		"oci://ghcr.io/azure/agent-azlinux3:v2",
	}, got)

	_, err = normalizeRootfsImages("ghcr.io/azure/agent-ubuntu2404@sha256:0000000000000000000000000000000000000000000000000000000000000000")
	require.ErrorContains(t, err, "must use a tag")
}

func TestNormalizeKubernetesVersions(t *testing.T) {
	got := normalizeKubernetesVersions("1.34.9, v1.35.0\n1.35.1")
	require.Equal(t, []string{"v1.34.9", "v1.35.0", "v1.35.1"}, got)
}

func TestResolveOCIPublishConfig(t *testing.T) {
	config, err := resolveOCIPublishConfig("GHCR.IO/Azure/Unbounded/", "v0.4.0")
	require.NoError(t, err)
	require.Equal(t, ociPublishConfig{
		registry:  "ghcr.io/azure/unbounded",
		tagPrefix: "v0.4.0",
	}, config)

	config, err = resolveOCIPublishConfig("", "")
	require.NoError(t, err)
	require.Equal(t, ociPublishConfig{}, config)

	_, err = resolveOCIPublishConfig("ghcr.io/azure/unbounded", "")
	require.ErrorContains(t, err, "--artifact-tag-prefix is required")

	_, err = resolveOCIPublishConfig("", "v0.4.0")
	require.ErrorContains(t, err, "--oci-registry is required")
}

func readGitHubOutput(t *testing.T, path string) map[string]string {
	t.Helper()

	data, err := os.ReadFile(path)
	require.NoError(t, err)

	values := map[string]string{}

	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		key, value, ok := strings.Cut(line, "=")
		require.Truef(t, ok, "invalid GitHub output line %q", line)

		values[key] = value
	}

	return values
}

func requireJSONEqual[T any](t *testing.T, expected T, raw string) {
	t.Helper()

	var got T
	require.NoError(t, json.Unmarshal([]byte(raw), &got))
	require.Equal(t, expected, got)
}
