// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package hostimage

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"
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
		"python3 /usr/local/lib/unbounded-image/verify_cloud_init_merge.py",
		`ARG SSH_IMPORT_ID_VERSION=5.11-0ubuntu2.24.04.1`,
		`ARG SSH_IMPORT_ID_SHA256=97bb2840a271ee0573da934ce1811f961e4b844fe876e34602b4661db25135b5`,
		"sha256sum --check --strict",
		"--ssh-import-id-package /ssh-import-id-package",
		"COPY images/netboot/assets/vendor-data.tmpl",
		"pigz -1 -n -c /work/disk.img > /disk/disk.img.gz",
		"COPY --from=build /disk /disk",
		`io.unbounded.host.qualification="blocked-bcho-has-no-github-ssh-keys"`,
	} {
		require.Contains(t, containerfile, value)
	}
	require.NotContains(t, containerfile, "images/host-ubuntu2404/Containerfile")
}

func TestHillsboroAccessPolicyMatchesVerifiedTeamSnapshot(t *testing.T) {
	var config struct {
		Users []struct {
			Name        string   `json:"name"`
			LockPasswd  bool     `json:"lock_passwd"`
			Shell       string   `json:"shell"`
			Sudo        string   `json:"sudo"`
			SSHImportID []string `json:"ssh_import_id"`
		} `json:"users"`
	}
	require.NoError(t, yaml.Unmarshal(
		[]byte(readRepoFile(t, imageDir+"/assets/90-hillsboro-users.cfg")),
		&config,
	))
	expectedUsers := []string{
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
	}
	require.Len(t, config.Users, len(expectedUsers))
	for index, user := range config.Users {
		require.Equal(t, expectedUsers[index], user.Name)
		require.True(t, user.LockPasswd)
		require.Equal(t, "/bin/bash", user.Shell)
		require.Equal(t, "ALL=(ALL:ALL) NOPASSWD:ALL", user.Sudo)
		require.Equal(t, []string{"gh:" + user.Name}, user.SSHImportID)
	}
}

func TestHillsboroCustomizerFailsClosedAndGeneralizes(t *testing.T) {
	customizer := readRepoFile(t, imageDir+"/assets/customize.py")

	for _, value := range []string{
		`"/usr/bin/cloud-init"`,
		`"/usr/bin/ssh-import-id"`,
		"filesystem.install_tree(ssh_import_id_package)",
		`filesystem.remove_tree("/var/lib/cloud")`,
		`name.startswith("ssh_host_")`,
		`"/etc/machine-id"`,
		`"/var/lib/dbus/machine-id"`,
		"captured password credential present",
		"captured SSH credential present",
		`"/etc/cloud/cloud.cfg.d/90-hillsboro-users.cfg"`,
	} {
		require.Contains(t, customizer, value)
	}
	for _, forbidden := range []string{
		"/etc/sudoers.d/",
		"/etc/systemd/system/cloud-final.service.d/",
		"/usr/local/sbin/unbounded-hillsboro-access",
	} {
		require.NotContains(t, customizer, forbidden)
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
		"native cloud-init",
		"bcho",
		"qualification blocker",
		"`spec.host.netboot.cloudInit.userDataConfigMapRef: gb200-metal-host-access`",
		"must be removed or replaced",
		"go run ./hack/cmd/hostimage-userdata-preflight -",
	} {
		require.Contains(t, readme, value)
	}
}

func TestMetalmanVendorDataDoesNotReplaceImageUsers(t *testing.T) {
	vendorData := readRepoFile(t, "images/netboot/assets/vendor-data.tmpl")
	require.NotContains(t, vendorData, "\nusers:")

	mergeTest := readRepoFile(t, imageDir+"/assets/verify_cloud_init_merge.py")
	require.Contains(t, mergeTest, "from cloudinit import util")
	require.Contains(t, mergeTest, "util.mergemanydict([vendor_config, image_config])")
	require.Contains(t, mergeTest, `"users" in vendor_config`)
	require.Contains(t, mergeTest, `("reporting", "write_files", "runcmd")`)
}
