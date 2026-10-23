// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package hostimage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const imageDir = "images/host-ubuntu2404-gb200-hillsboro"

func repoRoot(t *testing.T) string {
	t.Helper()

	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok)

	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func readRepoFile(t *testing.T, path string) string {
	t.Helper()

	data, err := os.ReadFile(filepath.Join(repoRoot(t), path))
	require.NoError(t, err)

	return string(data)
}

func TestHillsboroImageIsPinnedArm64Derivative(t *testing.T) {
	containerfile := readRepoFile(t, imageDir+"/Containerfile")

	for _, value := range []string{
		"ghcr.io/azure/host-ubuntu2404-gb200-nvidia-580167@sha256:39f4fd5312307ee2ef7ec5e236e420faa5d08e663853800d86fb6a3e162181bb",
		"FROM --platform=linux/arm64 ${BASE_IMAGE} AS source",
		`test "${TARGETARCH}" = arm64`,
		"COPY --from=source /disk/disk.img.gz /input/disk.img.gz",
		"python3 /usr/local/lib/unbounded-image/customize.py",
		"visudo -cf /usr/local/lib/unbounded-image/90-unbounded-dev",
		`ARG SSH_IMPORT_ID_VERSION=5.11-0ubuntu2.24.04.1`,
		`ARG SSH_IMPORT_ID_SHA256=97bb2840a271ee0573da934ce1811f961e4b844fe876e34602b4661db25135b5`,
		"sha256sum --check --strict",
		"--ssh-import-id-package /ssh-import-id-package",
		"pigz -1 -n -c /work/disk.img > /disk/disk.img.gz",
		"COPY --from=build /disk /disk",
	} {
		require.Contains(t, containerfile, value)
	}
	require.NotContains(t, containerfile, "images/host-ubuntu2404/Containerfile")
}

func TestHillsboroAccessPolicyMatchesVerifiedTeamSnapshot(t *testing.T) {
	var policy struct {
		Users                 []string `json:"users"`
		QualificationBlockers []string `json:"qualificationBlockers"`
	}
	require.NoError(t, json.Unmarshal(
		[]byte(readRepoFile(t, imageDir+"/assets/access-policy.json")),
		&policy,
	))
	require.Equal(t, []string{
		"bcho",
		"cchildress",
		"chokevin",
		"imiller31",
		"jveski",
		"jwilder",
		"phealy",
		"plombardi89",
		"qike-ms",
		"vpatelsj",
	}, policy.Users)
	require.Contains(t, strings.Join(policy.QualificationBlockers, "\n"), "bcho")

	access := readRepoFile(t, imageDir+"/assets/hillsboro-access")
	sudoers := readRepoFile(t, imageDir+"/assets/90-unbounded-dev")
	for _, user := range policy.Users {
		require.Contains(t, access, "\n  "+user+"\n")
		require.Contains(t, sudoers, user+" ALL=(ALL:ALL) NOPASSWD:ALL")
	}
	require.Contains(t, access, `usermod --lock --shell /bin/bash "$user"`)
	require.Contains(t, access, `ssh-import-id -o "$output" "gh:$user"`)
	require.Contains(t, access, "GitHub publishes no usable SSH keys")
}

func TestHillsboroCustomizerFailsClosedAndGeneralizes(t *testing.T) {
	customizer := readRepoFile(t, imageDir+"/assets/customize.py")

	for _, value := range []string{
		`FORBIDDEN_ACCOUNTS = {"administrator", "unbounded", "unbounded-metal"}`,
		`"/usr/bin/cloud-init"`,
		`"/usr/bin/ssh-import-id"`,
		"filesystem.install_tree(ssh_import_id_package)",
		`filesystem.remove_tree("/var/lib/cloud")`,
		`name.startswith("ssh_host_")`,
		`"/etc/machine-id"`,
		`"/var/lib/dbus/machine-id"`,
		"captured password credential present",
		"captured SSH credential present",
		`"/etc/sudoers.d/90-unbounded-dev"`,
		`"/etc/systemd/system/cloud-final.service.d/90-hillsboro-access.conf"`,
	} {
		require.Contains(t, customizer, value)
	}
}

func TestHillsboroAssetsContainNoEmbeddedCredentials(t *testing.T) {
	root := filepath.Join(repoRoot(t), imageDir)
	require.NoError(t, filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		require.NoError(t, err)
		if info.IsDir() {
			return nil
		}
		contents, readErr := os.ReadFile(path)
		require.NoError(t, readErr)
		text := string(contents)
		require.NotContains(t, text, "BEGIN OPENSSH PRIVATE KEY")
		require.NotContains(t, text, "BEGIN RSA PRIVATE KEY")
		require.NotRegexp(t, `ssh-(?:ed25519|rsa) [A-Za-z0-9+/]{32,}`, text)

		return nil
	}))
}

func TestHillsboroDocumentationKeepsActivationSeparate(t *testing.T) {
	readme := readRepoFile(t, imageDir+"/README.md")

	for _, value := range []string{
		"docker buildx build",
		"--platform linux/arm64",
		"`spec.pxe.image`",
		"`spec.pxe.netbootImage` unchanged",
		"Updating the image reference alone is non-destructive",
		"separately authorized `HostReplace`",
		"bcho",
		"qualification-blocked",
	} {
		require.Contains(t, readme, value)
	}
}
