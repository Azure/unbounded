// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cargo

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Azure/unbounded/hack/cmd/notice/internal/notice"
	"github.com/Azure/unbounded/hack/cmd/notice/internal/testutil"
)

func TestCollectorCollectHermetic(t *testing.T) {
	root := t.TempDir()
	cargoHome := t.TempDir()
	testutil.WriteTree(t, root, map[string]string{
		"cmd/racer-dataplane/Cargo.toml": `[package]
name = "racer-dataplane"
[dependencies]
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
	writeCollectorFixtures(t, root, "1.2.3")

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

	versions, err := lockedDirectVersions(lock, "racer-dataplane", direct)
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
		"cmd/racer-dataplane/Cargo.toml": "[package]\nname = \"racer-dataplane\"\n[dependencies]\n",
		"cmd/racer-dataplane/Cargo.lock": "version = 4\n",
	})
	writeCollectorFixtures(t, root, "1.2.3")

	err := New(t.TempDir()).Precheck(root)
	if err == nil || !strings.Contains(err.Error(), "cargo fetch") {
		t.Fatalf("Precheck error = %v", err)
	}
}

func TestCollectorRejectsDuplicateRegistrySources(t *testing.T) {
	root := t.TempDir()
	cargoHome := t.TempDir()
	testutil.WriteTree(t, root, map[string]string{
		"cmd/racer-dataplane/Cargo.toml": "[package]\nname = \"racer-dataplane\"\n[dependencies]\nfoo = \"1\"\n",
		"cmd/racer-dataplane/Cargo.lock": `version = 4

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
	writeCollectorFixtures(t, root, "1.2.3")

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
		"cmd/racer-dataplane/Cargo.toml": "[package]\nname = \"racer-dataplane\"\n[dependencies]\nfoo = \"1\"\n",
		"cmd/racer-dataplane/Cargo.lock": `version = 4

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
	writeCollectorFixtures(t, root, "1.2.3")
	testutil.WriteTree(t, cargoHome, map[string]string{
		"registry/src/index/foo-1.2.3/LICENSE":        "Choose either LICENSE-APACHE or LICENSE-MIT, included alongside this index.\n",
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

func TestLicenseIndexRejectsUnrecognizedLicenseText(t *testing.T) {
	for _, paths := range [][]string{
		{"LICENSE"},
		{"LICENSE", "LICENSE-MIT"},
		{"LICENSE", "COPYING"},
	} {
		if licenseIndex("LICENSE", []byte("Unknown license terms"), paths) {
			t.Fatalf("unrecognized text accepted for %v", paths)
		}
	}
}

func TestCollectorUsesDeclaredLicenseWithoutLicenseFile(t *testing.T) {
	root := t.TempDir()
	cargoHome := t.TempDir()
	testutil.WriteTree(t, root, map[string]string{
		"cmd/racer-dataplane/Cargo.toml": "[package]\nname = \"racer-dataplane\"\n[dependencies]\nfoo = \"1\"\n",
		"cmd/racer-dataplane/Cargo.lock": `version = 4

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
	writeCollectorFixtures(t, root, "1.2.3")
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

func writeCollectorFixtures(t *testing.T, root, version string) {
	t.Helper()

	lock, err := os.ReadFile(filepath.Join(root, "cmd/racer-dataplane/Cargo.lock"))
	if err != nil {
		t.Fatal(err)
	}

	for _, member := range []string{"runtime", "alloc", "crypto", "http"} {
		testutil.WriteTree(t, root, map[string]string{
			"cmd/racer-dataplane/" + member + "/Cargo.toml": "[package]\nname = \"racer-" + member + "\"\n",
		})
		lock = append(lock, []byte("\n[[package]]\nname = \"racer-"+member+"\"\nversion = \"0.1.0\"\n")...)
	}

	testutil.WriteTree(t, root, map[string]string{
		"cmd/racer-dataplane/Cargo.lock": string(lock),
		"cmd/racer-loadgen/performance/Cargo.toml": `[package]
name = "racer-performance-control"
[dependencies]
racer-dataplane = { path = "../../racer-dataplane" }
foo = "1"
`,
		"cmd/racer-loadgen/performance/Cargo.lock": `[[package]]
name = "foo"
version = "` + version + `"
[[package]]
name = "racer-performance-control"
version = "0.1.0"
dependencies = [
 "foo",
 "racer-dataplane",
]
`,
	})
}

func TestCollectorRejectsConflictingDirectVersions(t *testing.T) {
	root := t.TempDir()
	testutil.WriteTree(t, root, map[string]string{
		"cmd/racer-dataplane/Cargo.toml": "[package]\nname = \"racer-dataplane\"\n[dependencies]\nfoo = \"1\"\n",
		"cmd/racer-dataplane/Cargo.lock": `[[package]]
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
	writeCollectorFixtures(t, root, "1.2.4")

	_, err := New(t.TempDir()).Collect(root)
	if err == nil || !strings.Contains(err.Error(), "conflicting direct versions") {
		t.Fatalf("Collect error = %v", err)
	}
}

func TestManifestPackageName(t *testing.T) {
	for _, tc := range []struct {
		manifest, want string
	}{
		{"[package]\nname = \"racer-performance-control\"\n", "racer-performance-control"},
		{"[dependencies]\nname = \"not-the-root\"\n", ""},
		{"[package]\nname = \"\"\n", ""},
	} {
		got, err := manifestPackageName(tc.manifest)
		if got != tc.want || (err != nil) != (tc.want == "") {
			t.Fatalf("manifestPackageName(%q) = %q, %v", tc.manifest, got, err)
		}
	}
}

func TestLockedDirectVersionsRejectsMissingRoot(t *testing.T) {
	_, err := lockedDirectVersions("[[package]]\nname = \"other\"\n", "racer-dataplane", nil)
	if err == nil || !strings.Contains(err.Error(), "racer-dataplane package not found") {
		t.Fatalf("lockedDirectVersions error = %v", err)
	}
}

func TestCollectorPrecheckRequiresEveryManifest(t *testing.T) {
	root := t.TempDir()
	testutil.WriteTree(t, root, map[string]string{
		"cmd/racer-dataplane/Cargo.toml": "[package]\nname = \"racer-dataplane\"\n",
		"cmd/racer-dataplane/Cargo.lock": "version = 4\n",
	})

	err := New(t.TempDir()).Precheck(root)
	if err == nil || !strings.Contains(err.Error(), "cmd/racer-loadgen/performance/Cargo.toml") {
		t.Fatalf("Precheck error = %v", err)
	}
}

func TestCollectorCollectWorkspaceMembers(t *testing.T) {
	root := t.TempDir()
	cargoHome := t.TempDir()
	testutil.WriteTree(t, root, map[string]string{
		"cmd/racer-dataplane/Cargo.toml": `[package]
name = "racer-dataplane"
[dependencies]
racer-runtime = { path = "runtime" }
racer-alloc = { path = "alloc" }
racer-crypto = { path = "crypto" }
http1 = { path = "http" }
`,
		"cmd/racer-dataplane/Cargo.lock": "[[package]]\nname = \"racer-dataplane\"\nversion = \"0.1.0\"\n",
	})
	writeCollectorFixtures(t, root, "1.2.3")

	// Only the members depend directly on these registry packages. Each member
	// selects an exact version from a workspace lock containing two versions.
	lock := "[[package]]\nname = \"racer-dataplane\"\nversion = \"0.1.0\"\n"
	want := map[string]string{"foo": "1.2.3"}

	for _, member := range []string{"runtime", "alloc", "crypto", "http"} {
		name := member + "-dep"
		want[name] = "1.2.3"
		testutil.WriteTree(t, root, map[string]string{
			"cmd/racer-dataplane/" + member + "/Cargo.toml": fmt.Sprintf("[package]\nname = %q\n[dependencies]\n%s = \"1\"\n", "racer-"+member, name),
		})
		lock += fmt.Sprintf(`
[[package]]
name = %q
version = "0.1.0"
dependencies = [
 %q,
]
[[package]]
name = %q
version = "1.2.3"
[[package]]
name = %q
version = "2.0.0"
`, "racer-"+member, name+" 1.2.3", name, name)
	}

	testutil.WriteTree(t, root, map[string]string{"cmd/racer-dataplane/Cargo.lock": lock})

	for name, version := range want {
		testutil.WriteTree(t, cargoHome, map[string]string{
			"registry/src/index/" + name + "-" + version + "/LICENSE": testutil.MITLicense("Copyright (c) 2026 Example"),
		})
	}

	c := New(cargoHome)
	if err := c.Precheck(root); err != nil {
		t.Fatalf("Precheck without member locks: %v", err)
	}

	// A stale adjacent member lock must not override the workspace lock.
	for _, member := range []string{"runtime", "alloc", "crypto", "http"} {
		testutil.WriteTree(t, root, map[string]string{
			"cmd/racer-dataplane/" + member + "/Cargo.lock": "invalid stale member lock\n",
		})
	}

	entries, err := c.Collect(root)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	got := map[string]string{}

	for _, entry := range entries {
		if len(entry.License) != 1 {
			t.Fatalf("licenses for %s = %#v", entry.Dependency, entry.License)
		}

		got[entry.Dependency] = entry.License[0].Link
	}

	for name, version := range want {
		want[name] = fmt.Sprintf("https://docs.rs/crate/%s/%s/source/LICENSE", name, version)
	}

	if !reflect.DeepEqual(got, want) || len(entries) != len(want) {
		t.Fatalf("entries = %#v, want links %v", entries, want)
	}
}

func TestCollectorRequiresWorkspaceInputs(t *testing.T) {
	for _, missing := range []string{
		"cmd/racer-dataplane/runtime/Cargo.toml",
		"cmd/racer-dataplane/alloc/Cargo.toml",
		"cmd/racer-dataplane/crypto/Cargo.toml",
		"cmd/racer-dataplane/http/Cargo.toml",
		"cmd/racer-dataplane/Cargo.lock",
	} {
		t.Run(missing, func(t *testing.T) {
			root := t.TempDir()
			testutil.WriteTree(t, root, map[string]string{
				"cmd/racer-dataplane/Cargo.toml": "[package]\nname = \"racer-dataplane\"\n",
				"cmd/racer-dataplane/Cargo.lock": "[[package]]\nname = \"racer-dataplane\"\nversion = \"0.1.0\"\n",
			})
			writeCollectorFixtures(t, root, "1.2.3")

			if err := os.Remove(filepath.Join(root, missing)); err != nil {
				t.Fatal(err)
			}

			c := New(t.TempDir())
			if err := c.Precheck(root); err == nil || !strings.Contains(err.Error(), missing) {
				t.Fatalf("Precheck error = %v, want missing %s", err, missing)
			}

			if _, err := c.Collect(root); err == nil || !strings.Contains(err.Error(), missing) {
				t.Fatalf("Collect error = %v, want missing %s", err, missing)
			}
		})
	}
}
