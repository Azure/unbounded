// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package noderoute

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testOptions(t *testing.T) Options {
	t.Helper()
	root := t.TempDir()

	options := Options{
		HostCertsDir:            filepath.Join(root, "certs.d"),
		HostContainerdConfig:    filepath.Join(root, "config.toml"),
		HostStateDir:            filepath.Join(root, "state"),
		ExpectedContainerdCerts: "/etc/containerd/certs.d",
	}
	if err := os.WriteFile(options.HostContainerdConfig, []byte(`[plugins."io.containerd.grpc.v1.cri".registry]
config_path = "/etc/containerd/certs.d"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	return options
}

func TestReconcileInstallsAndRemovesNewRoute(t *testing.T) {
	options := testOptions(t)
	registry := Registry{Host: "example.azurecr.io", Server: "https://example.azurecr.io"}

	if err := Reconcile(t.Context(), options, Config{Registries: []Registry{registry}}); err != nil {
		t.Fatalf("install route: %v", err)
	}

	if err := Check(options, Config{Registries: []Registry{registry}}); err != nil {
		t.Fatalf("check installed route: %v", err)
	}

	if err := Reconcile(t.Context(), options, Config{}); err != nil {
		t.Fatalf("remove route: %v", err)
	}

	if _, err := os.Stat(targetPath(options, registry.Host)); !os.IsNotExist(err) {
		t.Fatalf("route still exists after removal: %v", err)
	}
}

func TestReconcileRefusesExistingRouteWithoutReplacement(t *testing.T) {
	options := testOptions(t)
	registry := Registry{Host: "registry.example.com", Server: "https://registry.example.com"}

	target := targetPath(options, registry.Host)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(target, []byte("original\n"), 0o640); err != nil {
		t.Fatal(err)
	}

	err := Reconcile(t.Context(), options, Config{Registries: []Registry{registry}})
	if err == nil || !strings.Contains(err.Error(), "replacement explicitly enabled") {
		t.Fatalf("want existing-route refusal, got %v", err)
	}
}

func TestReconcileReplacesAndRestoresExistingRoute(t *testing.T) {
	options := testOptions(t)
	registry := Registry{
		Host:            "registry.example.com",
		Server:          "https://registry.example.com",
		ReplaceExisting: true,
	}
	target := targetPath(options, registry.Host)
	original := []byte("server = \"https://old.example.com\"\n")

	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(target, original, 0o640); err != nil {
		t.Fatal(err)
	}

	if err := Reconcile(t.Context(), options, Config{Registries: []Registry{registry}}); err != nil {
		t.Fatalf("replace route: %v", err)
	}

	if err := Reconcile(t.Context(), options, Config{}); err != nil {
		t.Fatalf("restore route: %v", err)
	}

	restored, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}

	if string(restored) != string(original) {
		t.Fatalf("restored content = %q, want %q", restored, original)
	}

	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}

	if info.Mode().Perm() != 0o640 {
		t.Fatalf("restored mode = %o, want 640", info.Mode().Perm())
	}
}

func TestReconcileRefusesConcurrentManagedRouteChange(t *testing.T) {
	options := testOptions(t)

	registry := Registry{Host: "registry.example.com", Server: "https://registry.example.com"}
	if err := Reconcile(t.Context(), options, Config{Registries: []Registry{registry}}); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(targetPath(options, registry.Host), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := Reconcile(context.Background(), options, Config{Registries: []Registry{registry}})
	if err == nil || !strings.Contains(err.Error(), "concurrently changed") {
		t.Fatalf("want concurrent-change refusal, got %v", err)
	}
}

func TestReconcileReinstallsRouteAfterOriginalIsRestored(t *testing.T) {
	options := testOptions(t)
	registry := Registry{Host: "registry.example.com", Server: "https://registry.example.com", ReplaceExisting: true}
	target := targetPath(options, registry.Host)
	original := []byte("server = \"https://old.example.com\"\n")

	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(target, original, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := Reconcile(t.Context(), options, Config{Registries: []Registry{registry}}); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(target, original, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := Reconcile(t.Context(), options, Config{Registries: []Registry{registry}}); err != nil {
		t.Fatalf("reinstall route: %v", err)
	}

	if err := Check(options, Config{Registries: []Registry{registry}}); err != nil {
		t.Fatalf("check reinstalled route: %v", err)
	}
}

func TestReconcileReinstallsRouteAfterManagedFileIsRemoved(t *testing.T) {
	options := testOptions(t)

	registry := Registry{Host: "registry.example.com", Server: "https://registry.example.com"}
	if err := Reconcile(t.Context(), options, Config{Registries: []Registry{registry}}); err != nil {
		t.Fatal(err)
	}

	if err := os.Remove(targetPath(options, registry.Host)); err != nil {
		t.Fatal(err)
	}

	if err := Reconcile(t.Context(), options, Config{Registries: []Registry{registry}}); err != nil {
		t.Fatalf("reinstall missing route: %v", err)
	}

	if err := Check(options, Config{Registries: []Registry{registry}}); err != nil {
		t.Fatalf("check reinstalled route: %v", err)
	}
}

func TestReconcileRepairsManagedModeAndCheckRejectsSymlink(t *testing.T) {
	options := testOptions(t)
	registry := Registry{Host: "registry.example.com", Server: "https://registry.example.com"}

	desired := Config{Registries: []Registry{registry}}
	if err := Reconcile(t.Context(), options, desired); err != nil {
		t.Fatal(err)
	}

	target := targetPath(options, registry.Host)
	if err := os.Chmod(target, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := Check(options, desired); err == nil || !strings.Contains(err.Error(), "mode is 600") {
		t.Fatalf("want mode readiness failure, got %v", err)
	}

	if err := Reconcile(t.Context(), options, desired); err != nil {
		t.Fatalf("repair mode: %v", err)
	}

	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}

	if info.Mode().Perm() != 0o644 {
		t.Fatalf("repaired mode = %o, want 644", info.Mode().Perm())
	}

	contents, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}

	outside := filepath.Join(t.TempDir(), "hosts.toml")
	if err := os.WriteFile(outside, contents, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := os.Symlink(outside, target); err != nil {
		t.Fatal(err)
	}

	if err := Check(options, desired); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("want symlink readiness failure, got %v", err)
	}
}

func TestCheckRejectsMissingOrPendingOwnershipState(t *testing.T) {
	options := testOptions(t)
	registry := Registry{Host: "registry.example.com", Server: "https://registry.example.com"}

	desired := Config{Registries: []Registry{registry}}
	if err := Reconcile(t.Context(), options, desired); err != nil {
		t.Fatal(err)
	}

	statePath := stateFile(options.HostStateDir, registry.Host)

	state, err := readState(statePath)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.Remove(statePath); err != nil {
		t.Fatal(err)
	}

	if err := Check(options, desired); err == nil || !strings.Contains(err.Error(), "no ownership state") {
		t.Fatalf("want missing-state readiness failure, got %v", err)
	}

	state.PendingManagedSHA256 = state.ManagedSHA256
	if err := writeJSONAtomic(statePath, state, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := Check(options, desired); err == nil || !strings.Contains(err.Error(), "has not converged") {
		t.Fatalf("want pending-state readiness failure, got %v", err)
	}
}

func TestReconcileReconstructsStateWithoutBackingUpManagedFile(t *testing.T) {
	options := testOptions(t)
	registry := Registry{Host: "registry.example.com", Server: "https://registry.example.com"}

	desired := Config{Registries: []Registry{registry}}
	if err := Reconcile(t.Context(), options, desired); err != nil {
		t.Fatal(err)
	}

	if err := os.RemoveAll(options.HostStateDir); err != nil {
		t.Fatal(err)
	}

	if err := Reconcile(t.Context(), options, desired); err != nil {
		t.Fatalf("reconstruct ownership state: %v", err)
	}

	state, err := readState(stateFile(options.HostStateDir, registry.Host))
	if err != nil {
		t.Fatal(err)
	}

	if state.OriginalPresent {
		t.Fatal("managed file was recorded as the original baseline")
	}

	if err := Reconcile(t.Context(), options, Config{}); err != nil {
		t.Fatalf("remove reconstructed route: %v", err)
	}

	if _, err := os.Stat(targetPath(options, registry.Host)); !os.IsNotExist(err) {
		t.Fatalf("managed route still exists after removal: %v", err)
	}
}

func TestReconcileAdoptsOSReplacementAsNewBaseline(t *testing.T) {
	options := testOptions(t)
	registry := Registry{
		Host:            "registry.example.com",
		Server:          "https://registry.example.com",
		ReplaceExisting: true,
	}

	target := targetPath(options, registry.Host)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(target, []byte("old os hosts\n"), 0o640); err != nil {
		t.Fatal(err)
	}

	if err := Reconcile(t.Context(), options, Config{Registries: []Registry{registry}}); err != nil {
		t.Fatal(err)
	}

	newOSHosts := []byte("new os hosts\n")
	if err := os.WriteFile(target, newOSHosts, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.Chmod(target, 0o600); err != nil {
		t.Fatal(err)
	}

	registry.ManageReplacements = true
	if err := Reconcile(t.Context(), options, Config{Registries: []Registry{registry}}); err != nil {
		t.Fatalf("adopt OS replacement: %v", err)
	}

	if err := Reconcile(t.Context(), options, Config{}); err != nil {
		t.Fatalf("restore adopted OS replacement: %v", err)
	}

	restored, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(restored, newOSHosts) {
		t.Fatalf("restored content = %q, want %q", restored, newOSHosts)
	}

	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}

	if info.Mode().Perm() != 0o600 {
		t.Fatalf("restored mode = %o, want 600", info.Mode().Perm())
	}
}

func TestReconcileAdoptsOSReplacementModeChange(t *testing.T) {
	options := testOptions(t)
	registry := Registry{Host: "registry.example.com", Server: "https://registry.example.com", ReplaceExisting: true}
	target := targetPath(options, registry.Host)
	original := []byte("os hosts\n")

	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(target, original, 0o640); err != nil {
		t.Fatal(err)
	}

	if err := Reconcile(t.Context(), options, Config{Registries: []Registry{registry}}); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(target, original, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.Chmod(target, 0o600); err != nil {
		t.Fatal(err)
	}

	registry.ManageReplacements = true
	if err := Reconcile(t.Context(), options, Config{Registries: []Registry{registry}}); err != nil {
		t.Fatalf("adopt replacement mode: %v", err)
	}

	if err := Reconcile(t.Context(), options, Config{}); err != nil {
		t.Fatalf("restore replacement mode: %v", err)
	}

	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}

	if info.Mode().Perm() != 0o600 {
		t.Fatalf("restored mode = %o, want 600", info.Mode().Perm())
	}
}

func TestReconcileReinstallsRouteAfterFullHostStateReset(t *testing.T) {
	options := testOptions(t)
	registry := Registry{Host: "registry.example.com", Server: "https://registry.example.com"}

	desired := Config{Registries: []Registry{registry}}
	if err := Reconcile(t.Context(), options, desired); err != nil {
		t.Fatal(err)
	}

	if err := os.RemoveAll(options.HostCertsDir); err != nil {
		t.Fatal(err)
	}

	if err := os.RemoveAll(options.HostStateDir); err != nil {
		t.Fatal(err)
	}

	newOSHosts := []byte("new image hosts\n")

	target := targetPath(options, registry.Host)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(target, newOSHosts, 0o644); err != nil {
		t.Fatal(err)
	}

	desired.Registries[0].ManageReplacements = true

	if err := Reconcile(t.Context(), options, desired); err != nil {
		t.Fatalf("reinstall after host reset: %v", err)
	}

	if err := Check(options, desired); err != nil {
		t.Fatalf("check reinstalled route: %v", err)
	}

	if err := Reconcile(t.Context(), options, Config{}); err != nil {
		t.Fatalf("restore new image baseline: %v", err)
	}

	restored, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(restored, newOSHosts) {
		t.Fatalf("restored content = %q, want %q", restored, newOSHosts)
	}
}

func TestReconcileRejectsSymlinkedRegistryDirectory(t *testing.T) {
	options := testOptions(t)

	outside := t.TempDir()
	if err := os.MkdirAll(options.HostCertsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.Symlink(outside, filepath.Join(options.HostCertsDir, "registry.example.com")); err != nil {
		t.Fatal(err)
	}

	err := Reconcile(t.Context(), options, Config{Registries: []Registry{{Host: "registry.example.com", Server: "https://registry.example.com"}}})
	if err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("want symlink refusal, got %v", err)
	}
}

func TestReconcileRejectsCompetingDefaultGantryRoute(t *testing.T) {
	options := testOptions(t)

	path := filepath.Join(options.HostCertsDir, "_default", "hosts.toml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, []byte(`[host."http://127.0.0.1:5000"]`), 0o644); err != nil {
		t.Fatal(err)
	}

	err := Reconcile(t.Context(), options, Config{Registries: []Registry{{Host: "registry.example.com", Server: "https://registry.example.com"}}})
	if err == nil || !strings.Contains(err.Error(), "another Gantry installation") {
		t.Fatalf("want competing-install refusal, got %v", err)
	}
}

func TestEmptyDesiredStateRestoresWithoutInstallPreflight(t *testing.T) {
	options := testOptions(t)

	registry := Registry{Host: "registry.example.com", Server: "https://registry.example.com"}
	if err := Reconcile(t.Context(), options, Config{Registries: []Registry{registry}}); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(options.HostContainerdConfig, []byte("version = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := Reconcile(t.Context(), options, Config{}); err != nil {
		t.Fatalf("restore with failed install preflight: %v", err)
	}

	if err := Check(options, Config{}); err != nil {
		t.Fatalf("check restored empty state: %v", err)
	}
}

func TestReconcileRemovesUncommittedStateDirectory(t *testing.T) {
	options := testOptions(t)

	orphan := filepath.Join(options.HostStateDir, strings.Repeat("a", 32))
	if err := os.MkdirAll(orphan, 0o700); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(orphan, "original-hosts.toml"), []byte("backup"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := Reconcile(t.Context(), options, Config{}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("orphan state still exists: %v", err)
	}
}

func TestReconcileRejectsInvalidCommittedState(t *testing.T) {
	options := testOptions(t)
	host := "registry.example.com"

	directory := stateDirectory(options.HostStateDir, host)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}

	state := routeState{Version: stateVersion + 1, Host: host, ManagedSHA256: strings.Repeat("a", 64)}
	if err := writeJSONAtomic(filepath.Join(directory, "state.json"), state, 0o600); err != nil {
		t.Fatal(err)
	}

	err := Reconcile(t.Context(), options, Config{})
	if err == nil || !strings.Contains(err.Error(), "invalid ownership metadata") {
		t.Fatalf("want invalid state refusal, got %v", err)
	}
}

func TestReconcileRejectsMissingOriginalBackupBeforeMutation(t *testing.T) {
	options := testOptions(t)
	registry := Registry{Host: "registry.example.com", Server: "https://registry.example.com", ReplaceExisting: true}

	target := targetPath(options, registry.Host)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(target, []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := Reconcile(t.Context(), options, Config{Registries: []Registry{registry}}); err != nil {
		t.Fatal(err)
	}

	if err := os.Remove(originalFile(options.HostStateDir, registry.Host)); err != nil {
		t.Fatal(err)
	}

	err := Reconcile(t.Context(), options, Config{Registries: []Registry{registry}})
	if err == nil || !strings.Contains(err.Error(), "read route backup") {
		t.Fatalf("want missing backup refusal, got %v", err)
	}
}

func TestNormalizeRegistryHost(t *testing.T) {
	if got, err := NormalizeRegistryHost("MyACR.azurecr.io:443"); err != nil || got != "myacr.azurecr.io:443" {
		t.Fatalf("NormalizeRegistryHost = %q, %v", got, err)
	}

	for _, invalid := range []string{"", ".", "..", "https://registry.example.com", "registry.example.com/repo", `registry.example.com\repo`, "user@registry.example.com"} {
		if _, err := NormalizeRegistryHost(invalid); err == nil {
			t.Errorf("NormalizeRegistryHost(%q) unexpectedly succeeded", invalid)
		}
	}
}

func TestContainerdUsesCertsDir(t *testing.T) {
	for _, plugin := range []string{"io.containerd.grpc.v1.cri", "io.containerd.cri.v1.images"} {
		data := []byte("[plugins.\"" + plugin + "\".registry]\nconfig_path = '/etc/containerd/certs.d' # managed\n")

		matched, err := containerdUsesCertsDir(data, "/etc/containerd/certs.d")
		if err != nil {
			t.Fatalf("containerdUsesCertsDir(%s): %v", plugin, err)
		}

		if !matched {
			t.Fatalf("expected config_path match for %s", plugin)
		}
	}

	wrongTable := []byte("[debug]\nconfig_path = '/etc/containerd/certs.d'\n")

	matched, err := containerdUsesCertsDir(wrongTable, "/etc/containerd/certs.d")
	if err != nil {
		t.Fatalf("containerdUsesCertsDir(wrong table): %v", err)
	}

	if matched {
		t.Fatal("config_path from an unrelated TOML table matched")
	}
}
