// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cargo

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Azure/unbounded/hack/cmd/notice/internal/notice"
	"github.com/Azure/unbounded/hack/cmd/notice/internal/testutil"
)

func TestCollectorAbsentCargoFiles(t *testing.T) {
	for _, placeholder := range []bool{false, true} {
		name := "missing directory"
		if placeholder {
			name = "placeholder directory"
		}

		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			if placeholder {
				testutil.WriteTree(t, root, map[string]string{"cmd/racer-dataplane/README.md": "Placeholder"})
			}

			c := New(filepath.Join(t.TempDir(), "nonexistent-cache"))
			if err := c.Precheck(root); err != nil {
				t.Fatalf("Precheck: %v", err)
			}

			entries, err := c.Collect(root)
			if err != nil || len(entries) != 0 {
				t.Fatalf("Collect = %v, %v; want no entries and no error", entries, err)
			}
		})
	}
}

func TestCollectorRejectsIncompleteCargoFiles(t *testing.T) {
	for _, present := range []string{"Cargo.toml", "Cargo.lock"} {
		t.Run(present, func(t *testing.T) {
			root := t.TempDir()
			testutil.WriteTree(t, root, map[string]string{filepath.Join(cratePath, present): ""})

			c := New(t.TempDir())
			if err := c.Precheck(root); err == nil || !strings.Contains(err.Error(), "missing") {
				t.Fatalf("Precheck error = %v; want missing file error", err)
			}

			if _, err := c.Collect(root); err == nil || !strings.Contains(err.Error(), "missing") {
				t.Fatalf("Collect error = %v; want missing file error", err)
			}
		})
	}
}

func TestCollectorRejectsCargoFilesystemErrors(t *testing.T) {
	for _, name := range []string{"Cargo.toml", "Cargo.lock"} {
		for _, kind := range []string{"directory", "symlink loop", "dangling symlink"} {
			t.Run(name+"/"+kind, func(t *testing.T) {
				root := t.TempDir()
				testutil.WriteTree(t, root, map[string]string{filepath.Join(cratePath, "README.md"): "Placeholder"})
				path := filepath.Join(root, cratePath, name)

				var err error

				switch kind {
				case "directory":
					err = os.Mkdir(path, 0o755)
				case "dangling symlink":
					err = os.Symlink("missing", path)
				default:
					err = os.Symlink(name, path)
				}

				if err != nil {
					t.Fatal(err)
				}

				c := New(t.TempDir())
				if err := c.Precheck(root); err == nil {
					t.Fatal("expected Precheck filesystem error")
				}

				if _, err := c.Collect(root); err == nil {
					t.Fatal("expected Collect filesystem error")
				}
			})
		}
	}
}

func TestCollectorCollectHermetic(t *testing.T) {
	root := t.TempDir()
	cargoHome := t.TempDir()
	testutil.WriteTree(t, root, map[string]string{
		"cmd/racer-dataplane/Cargo.toml": `[dependencies]
foo = "1"

[build-dependencies]
build-helper = "2"

[target.'cfg(target_os = "linux")'.dependencies]
linux-only = "3"

[dev-dependencies]
test-only = "4"
`,
		"cmd/racer-dataplane/Cargo.lock": `version = 4

[[package]]
name = "foo"
version = "1.2.3"
source = "registry+https://github.com/rust-lang/crates.io-index"

[[package]]
name = "build-helper"
version = "2.0.1"
source = "registry+https://github.com/rust-lang/crates.io-index"

[[package]]
name = "linux-only"
version = "3.4.5"
source = "registry+https://github.com/rust-lang/crates.io-index"

[[package]]
name = "test-only"
version = "4.0.0"

[[package]]
name = "racer-dataplane"
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
name = "racer-dataplane"
version = "0.1.0"
dependencies = [
 "rand 0.8.6",
]
`

	versions, err := lockedDirectVersions(lock, direct, crateName)
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

func TestCollectorPrecheckAndCollectWithoutDependencies(t *testing.T) {
	root := t.TempDir()
	testutil.WriteTree(t, root, map[string]string{
		"cmd/racer-dataplane/Cargo.toml": "[dependencies]\n",
		"cmd/racer-dataplane/Cargo.lock": `version = 4

[[package]]
name = "racer-dataplane"
version = "0.1.0"
`,
	})

	c := New(filepath.Join(t.TempDir(), "nonexistent-cache"))
	if err := c.Precheck(root); err != nil {
		t.Fatalf("Precheck: %v", err)
	}

	entries, err := c.Collect(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("Collect = %v, %v; want no entries and no error", entries, err)
	}
}

func TestCollectorPrecheckReportsMissingCache(t *testing.T) {
	root := t.TempDir()
	testutil.WriteTree(t, root, map[string]string{
		"cmd/racer-dataplane/Cargo.toml": "[dependencies]\nfoo = \"1\"\n",
		"cmd/racer-dataplane/Cargo.lock": `version = 4

[[package]]
name = "foo"
version = "1.2.3"
source = "registry+https://github.com/rust-lang/crates.io-index"

[[package]]
name = "racer-dataplane"
version = "0.1.0"
dependencies = [
 "foo",
]
`,
	})

	err := New(filepath.Join(t.TempDir(), "nonexistent-cache")).Precheck(root)
	if err == nil || !strings.Contains(err.Error(), "cargo fetch") {
		t.Fatalf("Precheck error = %v", err)
	}
}

func TestCollectorRejectsDuplicateRegistrySources(t *testing.T) {
	root := t.TempDir()
	cargoHome := t.TempDir()
	testutil.WriteTree(t, root, map[string]string{
		"cmd/racer-dataplane/Cargo.toml": "[dependencies]\nfoo = \"1\"\n",
		"cmd/racer-dataplane/Cargo.lock": `version = 4

[[package]]
name = "foo"
version = "1.2.3"
source = "registry+https://github.com/rust-lang/crates.io-index"

[[package]]
name = "racer-dataplane"
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
		"cmd/racer-dataplane/Cargo.toml": "[dependencies]\nfoo = \"1\"\n",
		"cmd/racer-dataplane/Cargo.lock": `version = 4

[[package]]
name = "foo"
version = "1.2.3"
source = "registry+https://github.com/rust-lang/crates.io-index"

[[package]]
name = "racer-dataplane"
version = "0.1.0"
dependencies = [
 "foo",
]
`,
	})
	testutil.WriteTree(t, cargoHome, map[string]string{
		"registry/src/index/foo-1.2.3/LICENSE-APACHE":  testutil.Apache2License(),
		"registry/src/index/foo-1.2.3/LICENSE-MIT":     testutil.MITLicense("Copyright (c) 2026 Example"),
		"registry/src/index/foo-1.2.3/LICENSE":         "MIT OR Apache-2.0\n",
		"registry/src/index/foo-1.2.3/Cargo.toml.orig": "[package]\nlicense = \"MIT OR Apache-2.0\"\n",
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
		"cmd/racer-dataplane/Cargo.toml": "[dependencies]\nfoo = \"1\"\n",
		"cmd/racer-dataplane/Cargo.lock": `version = 4

[[package]]
name = "foo"
version = "1.2.3"
source = "registry+https://github.com/rust-lang/crates.io-index"

[[package]]
name = "racer-dataplane"
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

func TestCollectorLicenseIndexFailsClosed(t *testing.T) {
	for _, tt := range []struct{ name, text, other string }{
		{"unrecognized license", "not a license", testutil.MITLicense("Copyright (c) 2026 Example")},
		{"indexes without full text", "MIT OR Apache-2.0", "MIT OR Apache-2.0"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			testutil.WriteTree(t, home, map[string]string{
				"registry/src/index/foo-1.0.0/Cargo.toml.orig": "[package]\nlicense = \"MIT OR Apache-2.0\"\n",
				"registry/src/index/foo-1.0.0/LICENSE":         tt.text,
				"registry/src/index/foo-1.0.0/LICENSE-MIT":     tt.other,
			})

			if _, err := New(home).buildEntry("foo", "1.0.0"); err == nil {
				t.Fatal("expected license classification error")
			}
		})
	}
}

func TestCollectorFollowsLocalDependencies(t *testing.T) {
	for _, section := range []string{"dependencies", "build-dependencies", `target.'cfg(unix)'.dependencies`, `target.'cfg(unix)'.build-dependencies`} {
		t.Run(section, func(t *testing.T) {
			root := t.TempDir()
			cargoHome := t.TempDir()
			testutil.WriteTree(t, root, map[string]string{
				cratePath + "/Cargo.toml": "[" + section + "]\nrenamed = { package = \"topology\", path = \"topology\" }\n[dev-dependencies]\nignored = { path = \"missing\" }\n",
				cratePath + "/topology/Cargo.toml": `[dependencies]
sha2 = "0.10"
shared = { path = "../shared" }
[build-dependencies]
builder = { path = "../shared", package = "shared" }
[dev-dependencies]
futures = "0.3"
`,
				cratePath + "/shared/Cargo.toml": `[dependencies]
sha2 = "0.10"
# A cycle must not cause unbounded traversal.
topology = { path = "../topology" }
`,
				cratePath + "/Cargo.lock": `[[package]]
name = "racer-dataplane"
version = "0.1.0"
dependencies = [
 "topology",
]
[[package]]
name = "topology"
version = "0.1.0"
dependencies = [
 "sha2 0.10.9",
 "shared",
 "futures",
]
[[package]]
name = "shared"
version = "0.1.0"
dependencies = [
 "sha2 0.10.9",
 "topology",
]
[[package]]
name = "sha2"
version = "0.10.9"
source = "registry+https://github.com/rust-lang/crates.io-index"
dependencies = [
 "digest",
]
[[package]]
name = "digest"
version = "0.10.7"
source = "registry+https://github.com/rust-lang/crates.io-index"
[[package]]
name = "sha2"
version = "0.9.9"
[[package]]
name = "futures"
version = "0.3.31"
`,
			})
			testutil.WriteTree(t, cargoHome, map[string]string{
				"registry/src/index/sha2-0.10.9/LICENSE":   testutil.MITLicense("Copyright (c) 2026 Example"),
				"registry/src/index/digest-0.10.7/LICENSE": testutil.MITLicense("Copyright (c) 2026 Example"),
			})

			c := New(cargoHome)
			if err := c.Precheck(root); err != nil {
				t.Fatalf("Precheck: %v", err)
			}

			entries, err := c.Collect(root)
			if err != nil {
				t.Fatalf("Collect: %v", err)
			}

			if len(entries) != 2 {
				t.Fatalf("entries = %#v; want sha2 and digest", entries)
			}

			for _, entry := range entries {
				if entry.Dependency != "sha2" && entry.Dependency != "digest" {
					t.Fatalf("unexpected dependency %q", entry.Dependency)
				}

				if entry.Dependency == "sha2" && entry.License[0].Link != "https://docs.rs/crate/sha2/0.10.9/source/LICENSE" {
					t.Errorf("license link = %q", entry.License[0].Link)
				}
			}
		})
	}
}

func TestCollectorLocalDependencyFailures(t *testing.T) {
	const (
		rootLock = `[[package]]
name = "racer-dataplane"
version = "0.1.0"
dependencies = [
 "topology",
]
`
		topologyLock = `[[package]]
name = "topology"
version = "0.1.0"
dependencies = [
 "sha2",
]
`
		shaLock = `[[package]]
name = "sha2"
version = "0.10.9"
source = "registry+https://github.com/rust-lang/crates.io-index"
`
	)

	for _, tt := range []struct {
		name     string
		manifest string
		lock     string
		want     string
	}{
		{name: "missing manifest", lock: rootLock + topologyLock, want: "topology/Cargo.toml"},
		{name: "malformed dependency", manifest: "[dependencies]\ninvalid\n", lock: rootLock + topologyLock, want: "invalid dependency line"},
		{name: "missing local lock entry", manifest: "[dependencies]\n", lock: rootLock, want: "dependency topology has no locked version"},
		{name: "missing registry lock entry", manifest: "[dependencies]\nsha2 = \"0.10\"\n", lock: rootLock + topologyLock, want: "dependency sha2 has no locked version"},
		{name: "missing dependency edge", manifest: "[dependencies]\nsha2 = \"0.10\"\n", lock: rootLock + "[[package]]\nname = \"topology\"\nversion = \"0.1.0\"\n" + shaLock, want: "direct dependency sha2 not found in root lock entry"},
		{name: "ambiguous registry version", manifest: "[dependencies]\nsha2 = \"0.10\"\n", lock: rootLock + topologyLock + shaLock + "[[package]]\nname = \"sha2\"\nversion = \"0.9.9\"\n", want: "dependency sha2 has ambiguous locked versions"},
		{name: "missing registry cache", manifest: "[dependencies]\nsha2 = \"0.10\"\n", lock: rootLock + topologyLock + shaLock, want: "registry source directory not found"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()

			files := map[string]string{
				cratePath + "/Cargo.toml": "[dependencies]\ntopology = { path = \"topology\" }\n",
				cratePath + "/Cargo.lock": tt.lock,
			}
			if tt.manifest != "" {
				files[cratePath+"/topology/Cargo.toml"] = tt.manifest
			}

			testutil.WriteTree(t, root, files)

			c := New(filepath.Join(t.TempDir(), "nonexistent-cache"))
			if tt.name == "missing registry cache" {
				if err := c.Precheck(root); err == nil || !strings.Contains(err.Error(), "cargo fetch") {
					t.Fatalf("Precheck error = %v; want fetch guidance", err)
				}
			}

			_, err := c.Collect(root)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Collect error = %v; want %q", err, tt.want)
			}
		})
	}
}

func TestCollectorLocalOnlyDependencies(t *testing.T) {
	root := t.TempDir()
	testutil.WriteTree(t, root, map[string]string{
		cratePath + "/Cargo.toml":          "[dependencies]\ntopology = { path = \"topology\" }\n",
		cratePath + "/topology/Cargo.toml": "[dev-dependencies]\nfutures = \"0.3\"\n",
		cratePath + "/Cargo.lock": `[[package]]
name = "racer-dataplane"
version = "0.1.0"
dependencies = [
 "topology",
]
[[package]]
name = "topology"
version = "0.1.0"
dependencies = [
 "futures",
]
`,
	})

	c := New(filepath.Join(t.TempDir(), "nonexistent-cache"))
	if err := c.Precheck(root); err != nil {
		t.Fatalf("Precheck local-only graph: %v", err)
	}

	entries, err := c.Collect(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("Collect = %v, %v; want no entries and no error", entries, err)
	}
}

func TestCollectorRejectsConflictingLocalRegistryVersions(t *testing.T) {
	root := t.TempDir()
	testutil.WriteTree(t, root, map[string]string{
		cratePath + "/Cargo.toml":          "[dependencies]\ntopology = { path = \"topology\" }\nsha2 = \"0.9\"\n",
		cratePath + "/topology/Cargo.toml": "[dependencies]\nsha2 = \"0.10\"\n",
		cratePath + "/Cargo.lock": `[[package]]
name = "racer-dataplane"
version = "0.1.0"
dependencies = [
 "topology",
 "sha2 0.9.9",
]
[[package]]
name = "topology"
version = "0.1.0"
dependencies = [
 "sha2 0.10.9",
]
[[package]]
name = "sha2"
version = "0.9.9"
[[package]]
name = "sha2"
version = "0.10.9"
`,
	})

	_, err := New(t.TempDir()).Collect(root)
	if err == nil || !strings.Contains(err.Error(), "dependency sha2 has ambiguous locked versions") {
		t.Fatalf("Collect error = %v; want conflicting registry version error", err)
	}
}
