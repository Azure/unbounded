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
		"cmd/unbounded-storage/Cargo.toml": `[dependencies]
foo = "1"

[build-dependencies]
build-helper = "2"

[target.'cfg(target_os = "linux")'.dependencies]
linux-only = "3"

[dev-dependencies]
test-only = "4"
`,
		"cmd/unbounded-storage/Cargo.lock": `version = 4

[[package]]
name = "foo"
version = "1.2.3"

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
name = "unbounded-storage"
version = "0.1.0"
dependencies = [
 "build-helper",
 "foo",
 "linux-only",
 "test-only",
]
`,
	})

	for _, crate := range []string{"foo-1.2.3", "build-helper-2.0.1", "linux-only-3.4.5"} {
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

	if len(entries) != 3 {
		t.Fatalf("got %d entries, want 3", len(entries))
	}

	byDependency := make(map[string]notice.Entry, len(entries))
	for _, entry := range entries {
		byDependency[entry.Dependency] = entry
	}

	for _, dependency := range []string{"build-helper", "foo", "linux-only"} {
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
name = "unbounded-storage"
version = "0.1.0"
dependencies = [
 "rand 0.8.6",
]
`

	versions, err := lockedDirectVersions(lock, direct)
	if err != nil {
		t.Fatalf("lockedDirectVersions: %v", err)
	}

	if versions["rand"] != "0.8.6" {
		t.Errorf("rand version = %q", versions["rand"])
	}
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
		"cmd/unbounded-storage/Cargo.toml": "[dependencies]\n",
		"cmd/unbounded-storage/Cargo.lock": "version = 4\n",
	})

	err := New(t.TempDir()).Precheck(root)
	if err == nil || !strings.Contains(err.Error(), "cargo fetch") {
		t.Fatalf("Precheck error = %v", err)
	}
}

func TestCollectorRejectsDuplicateRegistrySources(t *testing.T) {
	root := t.TempDir()
	cargoHome := t.TempDir()
	testutil.WriteTree(t, root, map[string]string{
		"cmd/unbounded-storage/Cargo.toml": "[dependencies]\nfoo = \"1\"\n",
		"cmd/unbounded-storage/Cargo.lock": `version = 4

[[package]]
name = "foo"
version = "1.2.3"

[[package]]
name = "unbounded-storage"
version = "0.1.0"
dependencies = [
 "foo",
]
`,
	})

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
	root := t.TempDir()
	cargoHome := t.TempDir()
	testutil.WriteTree(t, root, map[string]string{
		"cmd/unbounded-storage/Cargo.toml": "[dependencies]\nfoo = \"1\"\n",
		"cmd/unbounded-storage/Cargo.lock": `version = 4

[[package]]
name = "foo"
version = "1.2.3"

[[package]]
name = "unbounded-storage"
version = "0.1.0"
dependencies = [
 "foo",
]
`,
	})
	testutil.WriteTree(t, cargoHome, map[string]string{
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

func TestCollectorUsesDeclaredLicenseWithoutLicenseFile(t *testing.T) {
	root := t.TempDir()
	cargoHome := t.TempDir()
	testutil.WriteTree(t, root, map[string]string{
		"cmd/unbounded-storage/Cargo.toml": "[dependencies]\nfoo = \"1\"\n",
		"cmd/unbounded-storage/Cargo.lock": `version = 4

[[package]]
name = "foo"
version = "1.2.3"

[[package]]
name = "unbounded-storage"
version = "0.1.0"
dependencies = [
 "foo",
]
`,
	})
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
