// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package nodeconfig

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// aksConfig is the configuration an AKS node ships with. It is here verbatim
// because it is the one shape that has to keep working: it declares version 2
// while using the containerd 2.x plugin names, which is exactly the case a
// version-based merge would get wrong.
const aksConfig = `version = 2
oom_score = -999
[plugins."io.containerd.cri.v1.images"]
  [plugins."io.containerd.cri.v1.images".pinned_images]
    sandbox = "mcr.microsoft.com/oss/v2/kubernetes/pause:3.6"
  [plugins."io.containerd.cri.v1.images".registry]
    config_path = "/etc/containerd/certs.d"
[plugins."io.containerd.cri.v1.runtime".containerd]
    default_runtime_name = "runc"
    [plugins."io.containerd.cri.v1.runtime".containerd.runtimes.runc]
      runtime_type = "io.containerd.runc.v2"
    [plugins."io.containerd.cri.v1.runtime".containerd.runtimes.runc.options]
      BinaryName = "/usr/bin/runc"
      SystemdCgroup = true
[metrics]
  address = "0.0.0.0:10257"
`

func parse(t *testing.T, text string) Document {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	doc, err := Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	return doc
}

func TestLoadMissingFileIsEmpty(t *testing.T) {
	t.Parallel()

	doc, err := Load(filepath.Join(t.TempDir(), "absent.toml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if len(doc) != 0 {
		t.Fatalf("expected empty document, got %v", doc)
	}
}

func TestApplyBootstrapCopiesDefaultRuntimeOptions(t *testing.T) {
	t.Parallel()

	doc := parse(t, aksConfig)

	changed, err := Apply(doc, PhaseBootstrap, Options{})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if !changed {
		t.Fatal("expected the first bootstrap apply to change the document")
	}

	base := []string{"plugins", "io.containerd.cri.v1.runtime", "containerd", "runtimes", DefaultBootstrapRuntime}

	if got := lookup(doc, path(base, "runtime_type")); got != "io.containerd.runc.v2" {
		t.Fatalf("runtime_type = %v, want io.containerd.runc.v2", got)
	}

	// The whole reason the handler is copied rather than written fresh: a
	// handler that lost SystemdCgroup would run containers under the wrong
	// cgroup driver, which the kubelet notices only much later.
	if got := lookup(doc, path(base, "options", "SystemdCgroup")); got != true {
		t.Fatalf("SystemdCgroup = %v, want true", got)
	}

	if got := lookup(doc, path(base, "options", "BinaryName")); got != "/usr/bin/runc" {
		t.Fatalf("BinaryName = %v, want /usr/bin/runc", got)
	}

	if got := lookup(doc, path(base, "snapshotter")); got != "overlayfs" {
		t.Fatalf("snapshotter = %v, want overlayfs", got)
	}

	if got := lookup(doc, []string{"proxy_plugins", "gantry", "type"}); got != "snapshot" {
		t.Fatalf("proxy plugin type = %v, want snapshot", got)
	}

	if got := lookup(doc, []string{"proxy_plugins", "gantry", "address"}); got != DefaultSocket {
		t.Fatalf("proxy plugin address = %v, want %s", got, DefaultSocket)
	}
}

func TestApplyBootstrapLeavesTheDefaultRuntimeAlone(t *testing.T) {
	t.Parallel()

	doc := parse(t, aksConfig)

	if _, err := Apply(doc, PhaseBootstrap, Options{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	runc := []string{"plugins", "io.containerd.cri.v1.runtime", "containerd", "runtimes", "runc"}
	if got := lookup(doc, path(runc, "snapshotter")); got != nil {
		t.Fatalf("runc handler gained a snapshotter %v", got)
	}

	// Nothing outside the merge's own keys may move.
	if got := lookup(doc, []string{"oom_score"}); got != int64(-999) {
		t.Fatalf("oom_score = %v, want -999", got)
	}

	sandbox := []string{"plugins", "io.containerd.cri.v1.images", "pinned_images", "sandbox"}
	if got := lookup(doc, sandbox); got != "mcr.microsoft.com/oss/v2/kubernetes/pause:3.6" {
		t.Fatalf("pinned sandbox image = %v", got)
	}
}

func TestApplyIsIdempotent(t *testing.T) {
	t.Parallel()

	for _, phase := range []Phase{PhaseBootstrap, PhaseDefault} {
		doc := parse(t, aksConfig)

		changed, err := Apply(doc, phase, Options{Platform: "linux/amd64"})
		if err != nil {
			t.Fatalf("first Apply(%d): %v", phase, err)
		}

		if !changed {
			t.Fatalf("first Apply(%d) reported no change", phase)
		}

		// A second pass reporting a change would restart containerd on every
		// tick of the agent that owns this file.
		changed, err = Apply(doc, phase, Options{Platform: "linux/amd64"})
		if err != nil {
			t.Fatalf("second Apply(%d): %v", phase, err)
		}

		if changed {
			t.Fatalf("second Apply(%d) reported a change", phase)
		}
	}
}

func TestApplyDefaultSetsImageKeys(t *testing.T) {
	t.Parallel()

	doc := parse(t, aksConfig)

	if _, err := Apply(doc, PhaseDefault, Options{Platform: "linux/amd64"}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	images := []string{"plugins", "io.containerd.cri.v1.images"}

	if got := lookup(doc, path(images, "snapshotter")); got != DefaultSnapshotter {
		t.Fatalf("snapshotter = %v, want %s", got, DefaultSnapshotter)
	}

	if got := lookup(doc, path(images, "disable_snapshot_annotations")); got != false {
		t.Fatalf("disable_snapshot_annotations = %v, want false", got)
	}

	if got := lookup(doc, path(images, "discard_unpacked_layers")); got != false {
		t.Fatalf("discard_unpacked_layers = %v, want false", got)
	}

	entries, ok := lookup(doc, []string{"plugins", "io.containerd.transfer.v1.local", "unpack_config"}).([]any)
	if !ok || len(entries) != 1 {
		t.Fatalf("unpack_config = %v, want one entry", entries)
	}

	row, _ := entries[0].(map[string]any)
	if row["platform"] != "linux/amd64" || row["snapshotter"] != DefaultSnapshotter {
		t.Fatalf("unpack_config entry = %v", row)
	}
}

func TestApplyDefaultRewritesAnExistingUnpackEntry(t *testing.T) {
	t.Parallel()

	doc := parse(t, aksConfig+`
[[plugins."io.containerd.transfer.v1.local".unpack_config]]
  platform = "linux/amd64"
  snapshotter = "overlayfs"
`)

	changed, err := Apply(doc, PhaseDefault, Options{Platform: "linux/amd64"})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if !changed {
		t.Fatal("expected a change")
	}

	entries, _ := lookup(doc, []string{"plugins", "io.containerd.transfer.v1.local", "unpack_config"}).([]any)
	if len(entries) != 1 {
		t.Fatalf("unpack_config = %v, want the entry rewritten in place", entries)
	}

	row, _ := entries[0].(map[string]any)
	if row["snapshotter"] != DefaultSnapshotter {
		t.Fatalf("unpack_config entry = %v", row)
	}
}

func TestApplyDefaultKeepsOtherPlatforms(t *testing.T) {
	t.Parallel()

	doc := parse(t, aksConfig+`
[[plugins."io.containerd.transfer.v1.local".unpack_config]]
  platform = "linux/arm64"
  snapshotter = "overlayfs"
`)

	if _, err := Apply(doc, PhaseDefault, Options{Platform: "linux/amd64"}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	entries, _ := lookup(doc, []string{"plugins", "io.containerd.transfer.v1.local", "unpack_config"}).([]any)
	if len(entries) != 2 {
		t.Fatalf("unpack_config = %v, want the arm64 entry kept", entries)
	}
}

func TestApplyUnifiedLayout(t *testing.T) {
	t.Parallel()

	doc := parse(t, `version = 2
[plugins."io.containerd.grpc.v1.cri".containerd]
  default_runtime_name = "runc"
  [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runc]
    runtime_type = "io.containerd.runc.v2"
    [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runc.options]
      SystemdCgroup = true
`)

	if _, err := Apply(doc, PhaseBootstrap, Options{}); err != nil {
		t.Fatalf("Apply(bootstrap): %v", err)
	}

	if _, err := Apply(doc, PhaseDefault, Options{}); err != nil {
		t.Fatalf("Apply(default): %v", err)
	}

	base := []string{"plugins", "io.containerd.grpc.v1.cri", "containerd"}

	if got := lookup(doc, path(base, "snapshotter")); got != DefaultSnapshotter {
		t.Fatalf("snapshotter = %v, want %s", got, DefaultSnapshotter)
	}

	handler := path(base, "runtimes", DefaultBootstrapRuntime)
	if got := lookup(doc, path(handler, "options", "SystemdCgroup")); got != true {
		t.Fatalf("SystemdCgroup = %v, want true", got)
	}
}

func TestApplyWithoutCRIPlugin(t *testing.T) {
	t.Parallel()

	doc := parse(t, "version = 2\n")

	if _, err := Apply(doc, PhaseBootstrap, Options{}); !errors.Is(err, ErrNoCRIPlugin) {
		t.Fatalf("Apply = %v, want ErrNoCRIPlugin", err)
	}
}

func TestApplyUnknownPhase(t *testing.T) {
	t.Parallel()

	doc := parse(t, aksConfig)

	if _, err := Apply(doc, Phase(7), Options{}); err == nil {
		t.Fatal("expected an error for an unknown phase")
	}
}

func TestApplyWithoutADefaultRuntimeNameFallsBackToRunc(t *testing.T) {
	t.Parallel()

	doc := parse(t, `version = 2
[plugins."io.containerd.cri.v1.runtime".containerd.runtimes.runc.options]
  SystemdCgroup = true
[plugins."io.containerd.cri.v1.runtime".containerd.runtimes.kata]
  runtime_type = "io.containerd.kata.v2"
`)

	if _, err := Apply(doc, PhaseBootstrap, Options{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// A node with a kata handler has not thereby made kata its default, so the
	// bootstrap handler must still be the runc one.
	base := []string{"plugins", "io.containerd.cri.v1.runtime", "containerd", "runtimes", DefaultBootstrapRuntime}
	if got := lookup(doc, path(base, "options", "SystemdCgroup")); got != true {
		t.Fatalf("SystemdCgroup = %v, want the runc handler copied", got)
	}

	if got := lookup(doc, path(base, "runtime_type")); got != nil {
		t.Fatalf("runtime_type = %v, want the kata handler untouched", got)
	}
}

func TestSaveWritesAnOriginalBackupOnce(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	if err := os.WriteFile(path, []byte(aksConfig), 0o600); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	doc, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if _, err := Apply(doc, PhaseBootstrap, Options{}); err != nil {
		t.Fatalf("Apply(bootstrap): %v", err)
	}

	if err := Save(path, doc); err != nil {
		t.Fatalf("Save: %v", err)
	}

	backup, err := os.ReadFile(path + ".gantry-orig")
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}

	if string(backup) != aksConfig {
		t.Fatalf("backup is not the original configuration:\n%s", backup)
	}

	if _, err := Apply(doc, PhaseDefault, Options{Platform: "linux/amd64"}); err != nil {
		t.Fatalf("Apply(default): %v", err)
	}

	if err := Save(path, doc); err != nil {
		t.Fatalf("second Save: %v", err)
	}

	// The point of the backup is recovering the node's original file, so a
	// later save must not replace it with a half-applied one.
	backup, err = os.ReadFile(path + ".gantry-orig")
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}

	if string(backup) != aksConfig {
		t.Fatalf("backup was overwritten:\n%s", backup)
	}
}

func TestSaveRoundTrips(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	if err := os.WriteFile(path, []byte(aksConfig), 0o600); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	doc, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	for _, phase := range []Phase{PhaseBootstrap, PhaseDefault} {
		if _, err := Apply(doc, phase, Options{Platform: "linux/amd64"}); err != nil {
			t.Fatalf("Apply(%d): %v", phase, err)
		}
	}

	if err := Save(path, doc); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Reloading the saved file and applying again must be a no-op, otherwise
	// every containerd restart begets another.
	reloaded, err := Load(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}

	for _, phase := range []Phase{PhaseBootstrap, PhaseDefault} {
		changed, err := Apply(reloaded, phase, Options{Platform: "linux/amd64"})
		if err != nil {
			t.Fatalf("reapply(%d): %v", phase, err)
		}

		if changed {
			t.Fatalf("reapply(%d) reported a change after a round trip", phase)
		}
	}
}
