// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package host

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// bundledProvenance is the provenance emitted by images/acl-nspawn-sysext for
// the systemd build shipped by Azure Container Linux 3.0.20260809.
const bundledProvenance = `SYSEXT_NAME=unbounded-nspawn
SYSTEMD_CONTAINER_NVR=systemd-container-255-33.azl3.x86_64
SYSTEMD_NVR=systemd-255-33.azl3.x86_64
BUNDLED_SHARED_LIB=usr/lib/systemd/libsystemd-shared-255-33.azl3.so
`

const unbundledProvenance = `SYSEXT_NAME=unbounded-nspawn
SYSTEMD_CONTAINER_NVR=systemd-container-255-27.azl3.x86_64
SYSTEMD_NVR=systemd-255-27.azl3.x86_64
BUNDLED_SHARED_LIB=
`

// aclHostVersion is what `systemctl --version` reports on
// MicrosoftCBLMariner:azure-linux-3:azure-linux-3-acl:latest.
const aclHostVersion = "systemd 255 (255-33.azl3)"

func TestParseSysextProvenance(t *testing.T) {
	t.Parallel()

	t.Run("bundled", func(t *testing.T) {
		t.Parallel()

		p, err := ParseSysextProvenance(bundledProvenance)
		require.NoError(t, err)
		assert.Equal(t, "unbounded-nspawn", p.Name)
		assert.Equal(t, "systemd-container-255-33.azl3.x86_64", p.SystemdContainerNVR)
		assert.Equal(t, "systemd-255-33.azl3.x86_64", p.SystemdNVR)
		assert.Equal(t, "usr/lib/systemd/libsystemd-shared-255-33.azl3.so", p.BundledSharedLib)
		assert.True(t, p.Bundled())
	})

	t.Run("unbundled has an empty library and reports not bundled", func(t *testing.T) {
		t.Parallel()

		p, err := ParseSysextProvenance(unbundledProvenance)
		require.NoError(t, err)
		assert.Empty(t, p.BundledSharedLib)
		assert.False(t, p.Bundled())
	})

	t.Run("comments blank lines and quotes are handled", func(t *testing.T) {
		t.Parallel()

		p, err := ParseSysextProvenance("# built by CI\n\nSYSEXT_NAME=\"ext\"\n  SYSTEMD_NVR = 'systemd-255-33.azl3.x86_64' \n")
		require.NoError(t, err)
		assert.Equal(t, "ext", p.Name)
		assert.Equal(t, "systemd-255-33.azl3.x86_64", p.SystemdNVR)
	})

	t.Run("lines without a separator are ignored", func(t *testing.T) {
		t.Parallel()

		p, err := ParseSysextProvenance("garbage\nSYSTEMD_NVR=systemd-255-33.azl3.x86_64\n")
		require.NoError(t, err)
		assert.Equal(t, "systemd-255-33.azl3.x86_64", p.SystemdNVR)
	})

	t.Run("missing SYSTEMD_NVR is an error", func(t *testing.T) {
		t.Parallel()

		_, err := ParseSysextProvenance("SYSEXT_NAME=ext\n")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "SYSTEMD_NVR")
	})

	t.Run("empty input is an error", func(t *testing.T) {
		t.Parallel()

		_, err := ParseSysextProvenance("")
		require.Error(t, err)
	})
}

func TestParseSystemdRelease(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		input       string
		wantMajor   int
		wantRelease string
		wantErr     bool
	}{
		{
			name:        "systemctl --version output",
			input:       aclHostVersion,
			wantMajor:   255,
			wantRelease: "33.azl3",
		},
		{
			name:        "package nvr with arch",
			input:       "systemd-255-33.azl3.x86_64",
			wantMajor:   255,
			wantRelease: "33.azl3",
		},
		{
			name:        "package nvr aarch64",
			input:       "systemd-255-27.azl3.aarch64",
			wantMajor:   255,
			wantRelease: "27.azl3",
		},
		{
			name:        "systemd-container nvr",
			input:       "systemd-container-255-30.azl3.x86_64",
			wantMajor:   255,
			wantRelease: "30.azl3",
		},
		{
			name:        "future major version",
			input:       "systemd 256 (256-1.azl3)",
			wantMajor:   256,
			wantRelease: "1.azl3",
		},
		{
			name:    "no release component",
			input:   "systemd 255",
			wantErr: true,
		},
		{
			name:    "unparseable",
			input:   "not a version",
			wantErr: true,
		},
		{
			name:    "empty",
			input:   "",
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := ParseSystemdRelease(tc.input)

			if tc.wantErr {
				require.Error(t, err)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tc.wantMajor, got.Major)
			assert.Equal(t, tc.wantRelease, got.Release)
		})
	}
}

func TestSystemdReleaseString(t *testing.T) {
	t.Parallel()

	r, err := ParseSystemdRelease(aclHostVersion)
	require.NoError(t, err)
	assert.Equal(t, "255-33.azl3", r.String())
}

func TestCheckSysextCompatibility(t *testing.T) {
	t.Parallel()

	bundled, err := ParseSysextProvenance(bundledProvenance)
	require.NoError(t, err)

	unbundled, err := ParseSysextProvenance(unbundledProvenance)
	require.NoError(t, err)

	t.Run("matching release is compatible", func(t *testing.T) {
		t.Parallel()

		require.NoError(t, CheckSysextCompatibility(bundled, aclHostVersion))
	})

	t.Run("unbundled with matching release is compatible", func(t *testing.T) {
		t.Parallel()

		require.NoError(t, CheckSysextCompatibility(unbundled, "systemd 255 (255-27.azl3)"))
	})

	t.Run("bundled tolerates a different release on the same major", func(t *testing.T) {
		t.Parallel()

		// Proven on a live host: an extension built from systemd-255-27.azl3 that
		// bundles libsystemd-shared-255-27.azl3.so runs on a 255-33.azl3 host,
		// because the release-qualified soname lets both libraries coexist.
		p := bundled
		p.SystemdNVR = "systemd-255-27.azl3.x86_64"
		p.BundledSharedLib = "usr/lib/systemd/libsystemd-shared-255-27.azl3.so"

		require.NoError(t, CheckSysextCompatibility(p, aclHostVersion))
	})

	t.Run("unbundled rejects a different release", func(t *testing.T) {
		t.Parallel()

		err := CheckSysextCompatibility(unbundled, aclHostVersion)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "does not bundle libsystemd-shared")
		assert.Contains(t, err.Error(), "255-27.azl3")
		assert.Contains(t, err.Error(), "255-33.azl3")
	})

	t.Run("major version mismatch is rejected even when bundled", func(t *testing.T) {
		t.Parallel()

		p := bundled
		p.SystemdNVR = "systemd-254-1.azl3.x86_64"
		p.BundledSharedLib = "usr/lib/systemd/libsystemd-shared-254-1.azl3.so"

		err := CheckSysextCompatibility(p, aclHostVersion)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "must be built for systemd 255")
	})

	t.Run("unparseable host version is an error", func(t *testing.T) {
		t.Parallel()

		err := CheckSysextCompatibility(bundled, "nonsense")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "host systemd version")
	})

	t.Run("unparseable extension version is an error", func(t *testing.T) {
		t.Parallel()

		p := bundled
		p.SystemdNVR = "nonsense"

		err := CheckSysextCompatibility(p, aclHostVersion)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "extension systemd version")
	})
}
