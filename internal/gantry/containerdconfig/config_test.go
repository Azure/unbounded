// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package containerdconfig

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

func TestApplyIsIdempotentAndRestoreIsExact(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "etc/containerd/config.toml")
	dropInPath := filepath.Join(root, "etc/containerd/conf.d/99-gantry-overlaybd.toml")
	original := []byte("version = 2\nroot = '/var/lib/containerd'\n")

	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(configPath, original, 0o644); err != nil {
		t.Fatal(err)
	}

	manager := Manager{
		ConfigPath:      configPath,
		DropInPath:      dropInPath,
		StateDir:        filepath.Join(root, "state"),
		Address:         "/run/gantry-overlaybd/gantry-overlaybd.sock",
		SnapshotterName: "gantry-overlaybd",
	}

	result, err := manager.Apply()
	if err != nil || !result.Changed {
		t.Fatalf("Apply = %+v, %v", result, err)
	}

	assertManagedConfig(t, configPath, dropInPath)

	result, err = manager.Apply()
	if err != nil || result.Changed {
		t.Fatalf("second Apply = %+v, %v", result, err)
	}

	result, err = manager.Restore()
	if err != nil || !result.Changed {
		t.Fatalf("Restore = %+v, %v", result, err)
	}

	if restored, err := os.ReadFile(configPath); err != nil || string(restored) != string(original) {
		t.Fatalf("restored config = %q, %v", restored, err)
	}

	if _, err := os.Stat(dropInPath); !os.IsNotExist(err) {
		t.Fatalf("drop-in remains: %v", err)
	}
}

func TestApplyUpdatesLegacyFilesystemReportingAndPreservesRestore(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "config.toml")

	original := []byte("version = 2\n")
	if err := os.WriteFile(configPath, original, 0o600); err != nil {
		t.Fatal(err)
	}

	manager := Manager{
		ConfigPath: configPath, DropInPath: filepath.Join(root, "conf.d/gantry.toml"),
		StateDir: filepath.Join(root, "state"), Address: "/run/gantry.sock", SnapshotterName: "gantry-overlaybd",
	}
	if _, err := manager.Apply(); err != nil {
		t.Fatal(err)
	}

	legacy := []byte("version = 2\n[proxy_plugins.gantry-overlaybd]\ntype = 'snapshot'\naddress = '/run/gantry.sock'\n")
	for _, path := range []string{manager.DropInPath, manager.statePath("managed-drop-in.toml")} {
		if err := os.WriteFile(path, legacy, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	manager.SnapshotterRoot = "/mnt/image-disk/gantry"

	result, err := manager.Apply()
	if err != nil || !result.Changed {
		t.Fatalf("upgrade Apply = %+v, %v", result, err)
	}

	data, err := os.ReadFile(manager.DropInPath)
	if err != nil {
		t.Fatal(err)
	}

	var document struct {
		ProxyPlugins map[string]struct {
			Exports map[string]string `toml:"exports"`
		} `toml:"proxy_plugins"`
	}
	if err := toml.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}

	if got := document.ProxyPlugins["gantry-overlaybd"].Exports["root"]; got != manager.SnapshotterRoot {
		t.Fatalf("filesystem root = %q, want %q", got, manager.SnapshotterRoot)
	}

	if result, err := manager.Apply(); err != nil || result.Changed {
		t.Fatalf("second Apply = %+v, %v", result, err)
	}

	if _, err := manager.Restore(); err != nil {
		t.Fatal(err)
	}

	if data, err := os.ReadFile(configPath); err != nil || string(data) != string(original) {
		t.Fatalf("restore = %q, %v", data, err)
	}
}

func TestApplyRejectsRelativeSnapshotterRoot(t *testing.T) {
	root := t.TempDir()

	manager := Manager{
		ConfigPath: filepath.Join(root, "config.toml"), DropInPath: filepath.Join(root, "gantry.toml"),
		StateDir: filepath.Join(root, "state"), Address: "/run/gantry.sock",
		SnapshotterName: "gantry-overlaybd", SnapshotterRoot: "relative/data",
	}
	if _, err := manager.Apply(); err == nil {
		t.Fatal("relative snapshotter filesystem root accepted")
	}

	if _, err := os.Stat(manager.StateDir); !os.IsNotExist(err) {
		t.Fatalf("invalid root changed managed state: %v", err)
	}
}

func TestRestorePreservesConcurrentChanges(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "config.toml")
	dropInPath := filepath.Join(root, "conf.d/99-gantry-overlaybd.toml")

	if err := os.WriteFile(configPath, []byte("version = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	manager := Manager{ConfigPath: configPath, DropInPath: dropInPath, StateDir: filepath.Join(root, "state"), Address: "/run/adapter.sock", SnapshotterName: "gantry-overlaybd"}
	if _, err := manager.Apply(); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(dropInPath, []byte("changed = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := manager.Restore()
	if err == nil || !result.Preserved {
		t.Fatalf("Restore = %+v, %v", result, err)
	}

	if data, readErr := os.ReadFile(dropInPath); readErr != nil || string(data) != "changed = true\n" {
		t.Fatalf("concurrent drop-in = %q, %v", data, readErr)
	}
}

func TestApplyRecoversInterruptedOriginalBackup(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "config.toml")
	dropInPath := filepath.Join(root, "conf.d/99-gantry-overlaybd.toml")
	stateDir := filepath.Join(root, "state")
	originalConfig := []byte("version = 2\n")
	originalDropIn := []byte("[proxy_plugins.overlaybd]\naddress = '/run/original.sock'\n")

	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}

	for path, data := range map[string][]byte{
		configPath: originalConfig,
		dropInPath: originalDropIn,
		filepath.Join(stateDir, "original-config.toml"): originalConfig,
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	manager := Manager{ConfigPath: configPath, DropInPath: dropInPath, StateDir: stateDir, Address: "/run/adapter.sock", SnapshotterName: "gantry-overlaybd"}
	if _, err := manager.Apply(); err != nil {
		t.Fatal(err)
	}

	if _, err := manager.Restore(); err != nil {
		t.Fatal(err)
	}

	if data, err := os.ReadFile(dropInPath); err != nil || string(data) != string(originalDropIn) {
		t.Fatalf("restored drop-in = %q, %v", data, err)
	}
}

func assertManagedConfig(t *testing.T, configPath, dropInPath string) {
	t.Helper()

	configData, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}

	var config struct {
		Imports []string `toml:"imports"`
		Root    string   `toml:"root"`
	}
	if err := toml.Unmarshal(configData, &config); err != nil {
		t.Fatal(err)
	}

	if len(config.Imports) != 1 || config.Imports[0] != importPattern || config.Root != "/var/lib/containerd" {
		t.Fatalf("managed config = %+v", config)
	}

	dropInData, err := os.ReadFile(dropInPath)
	if err != nil {
		t.Fatal(err)
	}

	var dropIn struct {
		Version      int `toml:"version"`
		ProxyPlugins map[string]struct {
			Type    string            `toml:"type"`
			Address string            `toml:"address"`
			Exports map[string]string `toml:"exports"`
		} `toml:"proxy_plugins"`
		Plugins map[string]struct {
			Containerd struct {
				Snapshotter string `toml:"snapshotter"`
			} `toml:"containerd"`
		} `toml:"plugins"`
	}
	if err := toml.Unmarshal(dropInData, &dropIn); err != nil {
		t.Fatal(err)
	}

	if dropIn.Version != 2 {
		t.Fatalf("managed drop-in version = %d, want 2", dropIn.Version)
	}

	plugin := dropIn.ProxyPlugins["gantry-overlaybd"]
	if plugin.Type != "snapshot" || plugin.Address != "/run/gantry-overlaybd/gantry-overlaybd.sock" ||
		plugin.Exports["root"] != DefaultSnapshotterRoot {
		t.Fatalf("managed drop-in = %+v", dropIn)
	}

	if got := dropIn.Plugins["io.containerd.grpc.v1.cri"].Containerd.Snapshotter; got != "gantry-overlaybd" {
		t.Fatalf("CRI snapshotter = %q, want gantry-overlaybd", got)
	}
}

func TestMarshalDropInVersion3(t *testing.T) {
	data, err := marshalDropIn([]byte("version = 3\n"), "/run/adapter.sock", "gantry-overlaybd", DefaultSnapshotterRoot)
	if err != nil {
		t.Fatal(err)
	}

	var dropIn struct {
		Version int `toml:"version"`
		Plugins map[string]struct {
			Snapshotter string `toml:"snapshotter"`
		} `toml:"plugins"`
	}
	if err := toml.Unmarshal(data, &dropIn); err != nil {
		t.Fatal(err)
	}

	if dropIn.Version != 3 {
		t.Fatalf("managed drop-in version = %d, want 3", dropIn.Version)
	}

	if got := dropIn.Plugins["io.containerd.cri.v1.images"].Snapshotter; got != "gantry-overlaybd" {
		t.Fatalf("CRI snapshotter = %q, want gantry-overlaybd", got)
	}
}

func TestMarshalDropInVersion2WithCRIImagesPlugin(t *testing.T) {
	config := []byte(`version = 2
[plugins."io.containerd.cri.v1.images"]
snapshotter = "overlaybd"
`)

	data, err := marshalDropIn(config, "/run/adapter.sock", "gantry-overlaybd", DefaultSnapshotterRoot)
	if err != nil {
		t.Fatal(err)
	}

	var dropIn struct {
		Version int `toml:"version"`
		Plugins map[string]struct {
			Snapshotter string `toml:"snapshotter"`
		} `toml:"plugins"`
	}
	if err := toml.Unmarshal(data, &dropIn); err != nil {
		t.Fatal(err)
	}

	if dropIn.Version != 2 {
		t.Fatalf("managed drop-in version = %d, want 2", dropIn.Version)
	}

	if got := dropIn.Plugins["io.containerd.cri.v1.images"].Snapshotter; got != "gantry-overlaybd" {
		t.Fatalf("CRI snapshotter = %q, want gantry-overlaybd", got)
	}
}

func TestMarshalDropInEnablesRemoteSnapshotPulls(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config string
		legacy bool
	}{
		{name: "legacy", config: "version = 2\n", legacy: true},
		{name: "modern", config: "version = 3\n"},
		{name: "modern plugin in legacy config", config: "version = 2\n[plugins.'io.containerd.cri.v1.images']\nsnapshotter = 'overlayfs'\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, err := marshalDropIn([]byte(tc.config), "/run/adapter.sock", "gantry-overlaybd", "/mnt/image-disk/gantry")
			if err != nil {
				t.Fatal(err)
			}

			var dropIn struct {
				ProxyPlugins map[string]struct {
					Exports map[string]string `toml:"exports"`
				} `toml:"proxy_plugins"`
				Plugins map[string]struct {
					DisableAnnotations *bool `toml:"disable_snapshot_annotations"`
					LocalPull          *bool `toml:"use_local_image_pull"`
					Containerd         struct {
						DisableAnnotations *bool `toml:"disable_snapshot_annotations"`
					} `toml:"containerd"`
				} `toml:"plugins"`
			}
			if err := toml.Unmarshal(data, &dropIn); err != nil {
				t.Fatal(err)
			}

			if got := dropIn.ProxyPlugins["gantry-overlaybd"].Exports["root"]; got != "/mnt/image-disk/gantry" {
				t.Fatalf("image filesystem root = %q, want the actual snapshotter disk", got)
			}

			images := dropIn.Plugins["io.containerd.cri.v1.images"]
			if images.LocalPull == nil || !*images.LocalPull {
				t.Fatal("custom snapshotter must use CRI-local pulls instead of unsupported transfer unpacking")
			}

			if images.DisableAnnotations == nil || *images.DisableAnnotations {
				t.Fatal("remote snapshot annotations must be explicitly enabled")
			}

			if tc.legacy {
				disabled := dropIn.Plugins["io.containerd.grpc.v1.cri"].Containerd.DisableAnnotations
				if disabled == nil || *disabled {
					t.Fatal("legacy CRI remote snapshot annotations must be explicitly enabled")
				}
			}
		})
	}
}
