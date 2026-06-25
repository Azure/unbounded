// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package storagesupervisor

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// clearConfigEnv unsets every environment variable LoadConfig consults so each
// test starts from a known state regardless of the host environment.
func clearConfigEnv(t *testing.T) {
	t.Helper()

	for _, name := range []string{
		"REPO", "VERSION", "PREFIX", "SERVICE_NAME", "CONFIG_PATH",
		"CONFIG_SOURCE_DIR", "STORAGE_ARGS", "HOST_ROOT", "SYSTEMCTL", "SOURCE", "LOCAL_TARBALL",
		"NO_ENABLE", "ARCH", "POOL_BYTES", "HUGEPAGES", "NODE_NAME", "STORAGE_RING_LABEL", "KUBECONFIG",
		"STORAGE_RDMA_INVENTORY_URL",
	} {
		t.Setenv(name, "")
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("ARCH", "amd64")

	cfg, err := LoadConfig()
	require.NoError(t, err)

	assert.Equal(t, defaultRepo, cfg.Repo)
	assert.Equal(t, defaultVersion, cfg.Version)
	assert.Equal(t, defaultPrefix, cfg.Prefix)
	assert.Equal(t, defaultServiceName, cfg.ServiceName)
	assert.Equal(t, defaultConfigPath, cfg.ConfigPath)
	assert.Equal(t, defaultSourceDir, cfg.SourceDir)
	assert.Equal(t, defaultHostRoot, cfg.HostRoot)
	assert.Equal(t, []string{"systemctl"}, cfg.Systemctl)
	assert.Equal(t, "amd64", cfg.Arch)
	assert.Equal(t, int64(defaultPoolBytes), cfg.PoolBytes)
	assert.Equal(t, int64(0), cfg.Hugepages)
	assert.False(t, cfg.NoEnable)
	assert.Equal(t, SourceRelease, cfg.SourceMode)
}

func TestLoadConfigOverrides(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("REPO", "acme/widgets")
	t.Setenv("VERSION", "v1.2.3")
	t.Setenv("PREFIX", "/srv/storage")
	t.Setenv("SERVICE_NAME", "mystorage")
	t.Setenv("CONFIG_PATH", "/etc/mystorage/cfg.binpb")
	t.Setenv("CONFIG_SOURCE_DIR", "/etc/mystorage-source")
	t.Setenv("STORAGE_ARGS", "--verbose --foo bar")
	t.Setenv("HOST_ROOT", "/host")
	t.Setenv("SYSTEMCTL", "nsenter -t 1 -m systemctl")
	t.Setenv("NO_ENABLE", "1")
	t.Setenv("ARCH", "aarch64")
	t.Setenv("POOL_BYTES", "262144000")
	t.Setenv("HUGEPAGES", "512")
	t.Setenv("NODE_NAME", "node-a")
	t.Setenv("STORAGE_RING_LABEL", "storage.example/ring")
	t.Setenv("KUBECONFIG", "/tmp/kubeconfig")
	t.Setenv("STORAGE_RDMA_INVENTORY_URL", "http://127.0.0.1:9100/inventory/rdma")

	cfg, err := LoadConfig()
	require.NoError(t, err)

	assert.Equal(t, "acme/widgets", cfg.Repo)
	assert.Equal(t, "v1.2.3", cfg.Version)
	assert.Equal(t, "/srv/storage", cfg.Prefix)
	assert.Equal(t, "mystorage", cfg.ServiceName)
	assert.Equal(t, "/etc/mystorage/cfg.binpb", cfg.ConfigPath)
	assert.Equal(t, "/etc/mystorage-source", cfg.SourceDir)
	assert.Equal(t, "--verbose --foo bar", cfg.StorageArgs)
	assert.Equal(t, "/host", cfg.HostRoot)
	assert.Equal(t, []string{"nsenter", "-t", "1", "-m", "systemctl"}, cfg.Systemctl)
	assert.True(t, cfg.NoEnable)
	assert.Equal(t, "arm64", cfg.Arch)
	assert.Equal(t, int64(262144000), cfg.PoolBytes)
	assert.Equal(t, int64(512), cfg.Hugepages)
	assert.Equal(t, "node-a", cfg.NodeName)
	assert.Equal(t, "storage.example/ring", cfg.StorageRingLabel)
	assert.Equal(t, "/tmp/kubeconfig", cfg.Kubeconfig)
	assert.Equal(t, "http://127.0.0.1:9100/inventory/rdma", cfg.RdmaInventoryURL)
}

func TestLoadConfigSourceClassification(t *testing.T) {
	tests := []struct {
		name   string
		source string
		local  string
		want   SourceMode
		wantS  string
	}{
		{name: "empty is release", want: SourceRelease},
		{name: "https url", source: "https://example.com/a.tar.gz", want: SourceURL, wantS: "https://example.com/a.tar.gz"},
		{name: "http url", source: "http://example.com/a.tar.gz", want: SourceURL, wantS: "http://example.com/a.tar.gz"},
		{name: "file path", source: "/tmp/a.tar.gz", want: SourceFile, wantS: "/tmp/a.tar.gz"},
		{name: "local tarball fallback", local: "/tmp/local.tar.gz", want: SourceFile, wantS: "/tmp/local.tar.gz"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearConfigEnv(t)
			t.Setenv("ARCH", "amd64")
			t.Setenv("SOURCE", tt.source)
			t.Setenv("LOCAL_TARBALL", tt.local)

			cfg, err := LoadConfig()
			require.NoError(t, err)

			assert.Equal(t, tt.want, cfg.SourceMode)
			assert.Equal(t, tt.wantS, cfg.Source)
		})
	}
}

func TestLoadConfigSourcePrecedence(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("ARCH", "amd64")
	t.Setenv("SOURCE", "https://example.com/a.tar.gz")
	t.Setenv("LOCAL_TARBALL", "/tmp/ignored.tar.gz")

	cfg, err := LoadConfig()
	require.NoError(t, err)

	assert.Equal(t, "https://example.com/a.tar.gz", cfg.Source)
	assert.Equal(t, SourceURL, cfg.SourceMode)
}

func TestLoadConfigInvalid(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
	}{
		{name: "bad arch", env: map[string]string{"ARCH": "mips"}},
		{name: "zero pool bytes", env: map[string]string{"ARCH": "amd64", "POOL_BYTES": "0"}},
		{name: "negative pool bytes", env: map[string]string{"ARCH": "amd64", "POOL_BYTES": "-5"}},
		{name: "non-numeric pool bytes", env: map[string]string{"ARCH": "amd64", "POOL_BYTES": "lots"}},
		{name: "negative hugepages", env: map[string]string{"ARCH": "amd64", "HUGEPAGES": "-1"}},
		{name: "non-numeric hugepages", env: map[string]string{"ARCH": "amd64", "HUGEPAGES": "many"}},
		{name: "empty systemctl", env: map[string]string{"ARCH": "amd64", "SYSTEMCTL": "   "}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearConfigEnv(t)

			for k, v := range tt.env {
				t.Setenv(k, v)
			}

			_, err := LoadConfig()
			assert.Error(t, err)
		})
	}
}

func TestResolveArch(t *testing.T) {
	tests := []struct {
		in   string
		want string
		ok   bool
	}{
		{in: "amd64", want: "amd64", ok: true},
		{in: "x86_64", want: "amd64", ok: true},
		{in: "arm64", want: "arm64", ok: true},
		{in: "aarch64", want: "arm64", ok: true},
		{in: "ppc64", ok: false},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := resolveArch(tt.in)
			if !tt.ok {
				assert.Error(t, err)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}
