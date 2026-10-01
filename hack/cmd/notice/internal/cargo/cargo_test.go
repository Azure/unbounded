// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cargo

import (
	"strings"
	"testing"

	"github.com/Azure/unbounded/hack/cmd/notice/internal/notice"
	"github.com/Azure/unbounded/hack/cmd/notice/internal/testutil"
)

func TestCollectorCollectHermetic(t *testing.T) {
	root := t.TempDir()
	cargoHome := t.TempDir()
	testutil.WriteTree(t, root, map[string]string{
		cratePath + "/Cargo.toml": `[package]
name = "renamed-root"
[dependencies]
foo = "1"
jemalloc_pprof = { version = "0.9", optional = true, features = ["symbolize"] }
tikv-jemallocator = { version = "0.7", optional = true, features = ["profiling_libunwind"] }
[build-dependencies]
build-helper = "2"
[target.'cfg(target_os = "linux")'.dependencies]
linux-only = "3"
[dev-dependencies]
test-only = "4"
[target.'cfg(target_os = "linux")'.dev-dependencies]
target-test-only = "4"
`,
		cratePath + "/Cargo.lock": `version = 4
[[package]]
name = "foo"
version = "1.2.3"
[[package]]
name = "jemalloc_pprof"
version = "0.9.0"
[[package]]
name = "tikv-jemallocator"
version = "0.7.0"
[[package]]
name = "build-helper"
version = "2.0.1"
[[package]]
name = "linux-only"
version = "3.4.5"
[[package]]
name = "test-only"
version = "4.0.0"
[[package]]
name = "target-test-only"
version = "4.0.0"
[[package]]
name = "renamed-root"
version = "0.1.0"
dependencies = [
 "build-helper",
 "foo",
 "jemalloc_pprof",
 "tikv-jemallocator",
 "linux-only",
 "test-only",
 "target-test-only",
]
`,
	})

	for _, crate := range []string{"foo-1.2.3", "build-helper-2.0.1", "linux-only-3.4.5", "jemalloc_pprof-0.9.0", "tikv-jemallocator-0.7.0"} {
		testutil.WriteTree(t, cargoHome, map[string]string{
			"registry/src/index/" + crate + "/Cargo.toml.orig": "[package]\nlicense = \"MIT\"\n",
			"registry/src/index/" + crate + "/LICENSE":         testutil.MITLicense("Copyright (c) 2026 Example"),
		})
	}

	c := New(cargoHome)
	if err := c.Precheck(root); err != nil {
		t.Fatalf("Precheck: %v", err)
	}

	entries, err := c.Collect(root)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	if len(entries) != 5 {
		t.Fatalf("got %d entries, want 5 (including optional production crates, excluding dev-only crates)", len(entries))
	}

	byDependency := make(map[string]notice.Entry, len(entries))
	for _, entry := range entries {
		byDependency[entry.Dependency] = entry
	}

	for _, dependency := range []string{"build-helper", "foo", "linux-only", "jemalloc_pprof", "tikv-jemallocator"} {
		if _, ok := byDependency[dependency]; !ok {
			t.Errorf("dependency %q not collected", dependency)
		}
	}

	if got := byDependency["foo"].License[0].Link; got != "https://docs.rs/crate/foo/1.2.3/source/LICENSE" {
		t.Errorf("license link = %q", got)
	}
}

func TestLockedDirectVersionsUsesQualifiedVersion(t *testing.T) {
	direct := map[string]dependency{"rand": {packageName: "rand"}}
	lock := `[[package]]
name = "rand"
version = "0.8.6"
[[package]]
name = "rand"
version = "0.9.2"
[[package]]
name = "racer-dataplane"
version = "0.1.0"
dependencies = [
 "rand 0.8.6",
]
`

	versions, err := lockedDirectVersions(lock, "racer-dataplane", direct)
	if err != nil {
		t.Fatalf("lockedDirectVersions: %v", err)
	}

	if versions["rand"] != "0.8.6" {
		t.Errorf("rand version = %q", versions["rand"])
	}

	t.Run("ambiguous", func(t *testing.T) {
		_, err := lockedDirectVersions(strings.ReplaceAll(lock, "rand 0.8.6", "rand"), "racer-dataplane", direct)
		if err == nil || !strings.Contains(err.Error(), "ambiguous") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("missing root", func(t *testing.T) {
		_, err := lockedDirectVersions(lock, "missing", direct)
		if err == nil || !strings.Contains(err.Error(), "missing package not found") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("missing dependency", func(t *testing.T) {
		_, err := lockedDirectVersions(lock, "racer-dataplane", map[string]dependency{"missing": {packageName: "missing"}})
		if err == nil || !strings.Contains(err.Error(), "not found in root lock entry") {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestDirectDependenciesResolvesPackageAlias(t *testing.T) {
	direct, err := directDependencies("[dependencies]\nrenamed = { package = \"actual-name\", version = \"1\" }\n")
	if err != nil {
		t.Fatalf("directDependencies: %v", err)
	}

	if got := direct["renamed"].packageName; got != "actual-name" {
		t.Errorf("package name = %q", got)
	}
}

func TestCollectorPrecheckReportsMissingCache(t *testing.T) {
	root := t.TempDir()
	testutil.WriteTree(t, root, map[string]string{
		cratePath + "/Cargo.toml": "[dependencies]\n",
		cratePath + "/Cargo.lock": "version = 4\n",
	})

	err := New(t.TempDir()).Precheck(root)
	if err == nil || !strings.Contains(err.Error(), "cargo fetch") {
		t.Fatalf("Precheck error = %v", err)
	}
}

func singleCrate(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	testutil.WriteTree(t, root, map[string]string{
		cratePath + "/Cargo.toml": "[package]\nname = \"racer-dataplane\"\n[dependencies]\nfoo = \"1\"\n",
		cratePath + "/Cargo.lock": `version = 4
[[package]]
name = "foo"
version = "1.2.3"
[[package]]
name = "racer-dataplane"
version = "0.1.0"
dependencies = [
 "foo",
]
`,
	})

	return root
}

func TestCollectorRejectsDuplicateRegistrySources(t *testing.T) {
	root := singleCrate(t)

	cargoHome := t.TempDir()
	for _, registry := range []string{"first", "second"} {
		testutil.WriteTree(t, cargoHome, map[string]string{
			"registry/src/" + registry + "/foo-1.2.3/LICENSE": testutil.MITLicense("Copyright (c) 2026 Example"),
		})
	}

	_, err := New(cargoHome).Collect(root)
	if err == nil || !strings.Contains(err.Error(), "multiple registry source directories") {
		t.Fatalf("Collect error = %v", err)
	}
}

func TestCollectorCollectsMultipleLicenseFiles(t *testing.T) {
	root := singleCrate(t)
	cargoHome := t.TempDir()
	testutil.WriteTree(t, cargoHome, map[string]string{
		"registry/src/index/foo-1.2.3/LICENSE":        "Choose the terms in LICENSE-APACHE or LICENSE-MIT.",
		"registry/src/index/foo-1.2.3/LICENSE-APACHE": testutil.Apache2License(),
		"registry/src/index/foo-1.2.3/LICENSE-MIT":    testutil.MITLicense("Copyright (c) 2026 Example"),
	})

	entries, err := New(cargoHome).Collect(root)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	if len(entries) != 1 || len(entries[0].License) != 2 {
		t.Fatalf("entries = %#v", entries)
	}

	if entries[0].License[0].Name != "Apache License, Version 2.0" || entries[0].License[1].Name != "MIT License" {
		t.Fatalf("licenses = %#v", entries[0].License)
	}
}

func TestCollectorRejectsUnknownLicense(t *testing.T) {
	root := singleCrate(t)
	cargoHome := t.TempDir()
	testutil.WriteTree(t, cargoHome, map[string]string{
		"registry/src/index/foo-1.2.3/LICENSE": "Unknown license terms",
	})

	_, err := New(cargoHome).Collect(root)
	if err == nil || !strings.Contains(err.Error(), "classifying") {
		t.Fatalf("Collect error = %v", err)
	}
}

func TestCollectorUsesDeclaredLicenseWithoutLicenseFile(t *testing.T) {
	root := singleCrate(t)
	cargoHome := t.TempDir()
	testutil.WriteTree(t, cargoHome, map[string]string{
		"registry/src/index/foo-1.2.3/Cargo.toml.orig": "[package]\nlicense = \"MIT OR Apache-2.0\"\n",
	})

	entries, err := New(cargoHome).Collect(root)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	if len(entries) != 1 || len(entries[0].License) != 2 {
		t.Fatalf("entries = %#v", entries)
	}

	if entries[0].License[0].Name != "MIT License" || entries[0].License[1].Name != "Apache License, Version 2.0" {
		t.Fatalf("licenses = %#v", entries[0].License)
	}
}
