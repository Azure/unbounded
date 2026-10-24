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

func TestHillsboroImageUsesPinnedARM64Producer(t *testing.T) {
	containerfile := readRepoFile(t, imageDir+"/Containerfile")

	for _, value := range []string{
		"ENV GB200_PROFILE=canonical-nvidia-580167",
		`test "${TARGETARCH}" = arm64`,
		"COPY images/host-ubuntu2404-gb200/assets/",
		"COPY images/host-ubuntu2404-gb200-hillsboro/assets/90-hillsboro-users.cfg",
		"python3 /usr/local/lib/unbounded-image/hillsboro/verify_cloud_init_merge.py",
		"COPY images/netboot/assets/vendor-data.tmpl",
		"ROOT_EXTRA_MIB=7168",
		"COPY --from=build /disk /disk",
	} {
		require.Contains(t, containerfile, value)
	}
	require.NotContains(t, containerfile, "images/host-ubuntu2404/Containerfile")
	require.NotContains(t, containerfile, "customize.py")
	require.NotContains(t, containerfile, "blocked-bcho")
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

func TestGB200ProducerInstallsSSHImportIDBeforeCapture(t *testing.T) {
	rootfs := readRepoFile(t, "images/host-ubuntu2404-gb200/assets/rootfs.sh")
	verify := readRepoFile(t, "images/host-ubuntu2404-gb200/assets/verify.py")

	require.Contains(t, rootfs, `"ssh-import-id=${SSH_IMPORT_ID_VERSION}"`)
	require.Contains(t, verify, `Path("/usr/bin/ssh-import-id").is_file()`)
	require.Contains(t, verify, `"missing pinned ssh-import-id"`)
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
		"Cloud-init still creates the locked `bcho` account",
		"key-based access until `bcho` publishes a key",
		"does not block image",
		"readiness or affect key-based access",
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

func TestHillsboroBootGateValidatesNamedAccess(t *testing.T) {
	boot := readRepoFile(t, "images/host-ubuntu2404-gb200/verify-boot.sh")
	install := readRepoFile(t, imageDir+"/assets/verify-boot-install.sh")
	importer := readRepoFile(t, imageDir+"/assets/verify-boot-ssh-import-id.sh")

	require.Contains(t, boot, "seed/vendor-data")
	require.Contains(t, boot, "render_boot_vendor_data.py")
	require.Contains(t, install, "test ! -s /home/bcho/.ssh/authorized_keys")
	for _, user := range []string{
		"cchildress",
		"chokevin",
		"imiller31",
		"jveski",
		"jwilder",
		"phealy",
		"plombardi89",
		"qike-ms",
		"vpatelsj",
	} {
		require.Contains(t, install, user)
	}
	require.Contains(t, install, `grep -Fq "gh:$user" "$keys"`)
	require.Contains(t, importer, `if [ "$user" = bcho ]`)
	require.Contains(t, importer, `> "$HOME/.ssh/authorized_keys"`)
}
